package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/pods"
	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PutWorkspaceBundle saves issue's workspace bundle as the work of release, replacing whatever the
// issue's last release left: one issue has one bundle, the newest release's, and a release that
// ran again after a wake cancelled the first must not leave the older work behind to be restored
// instead. release is the id of the idle release making the save (issue_releases.id), which the
// restore of the issue's next pod carries to that pod's launcher and back, so the report that
// confirms a restore names the release whose work it put back (DeleteRestoredWorkspaceBundle).
// The save clears the restore attempt the row carried with the rest of it: the work of this
// release is another bundle, and it gets an attempt of its own (TakeWorkspaceBundleForRestore).
//
// The bundle names the pod its work came off (runtime.WorkspaceBundle.PodUID), which the row
// keeps as source_pod_uid and the take refuses by: that pod's own workspace is where the work
// is, so a role relaunched in it must be answered nothing and must leave the attempt for the pod
// that replaces it. A save with no such pod is refused rather than stored, since the row could
// not then tell the released pod from its replacement.
//
// repo is the repository the saved work is of: the repository the released pod itself was built
// from, which is the repository its workspace is a clone of and so the one this work is in the
// history of, and the take refuses a bundle whose repository the issue has left by it
// (runtime.WorkspaceBundleRefusedRepository). It is an argument and not a column this statement
// reads, because the caller is what can read the pod (the daemon's
// outbox.bundleIssueWorkspace, over issuePodRepository): the record's own repository is not the
// same answer while a correction is waiting for the pod it moved to be replaced, and the pod's
// is the one the work is in. An empty repo is an issue the workflow does not record, since the
// caller falls back to the record's repository where the runtime answers none; it is stored as
// it is given and refuses nothing, which is what a row saved before this column reads as.
//
// The save ends the issue's bundle refusal in the same statement (RecordBundleRefusal): the
// work fit this time, so nothing is left for the idle rule to hold the pod for.
func (s *Store) PutWorkspaceBundle(ctx context.Context, issue string, release int64, repo string, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a workspace bundle needs its issue")
	}
	if release <= 0 {
		return fmt.Errorf("the workspace bundle of %s needs the release that saved it", issue)
	}
	if b.PodUID == "" {
		return fmt.Errorf("the workspace bundle of %s needs the uid of the pod its work came off", issue)
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	_, err = s.pool.Exec(ctx, `with ended as (delete from workspace_bundle_refusals where issue = $1)
		insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks, repo)
		values ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		on conflict (issue) do update set release_id = excluded.release_id, source_pod_uid = excluded.source_pod_uid,
			bundle = excluded.bundle, size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
			tips = excluded.tips, bookmarks = excluded.bookmarks, repo = excluded.repo,
			attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`,
		issue, release, b.PodUID, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks, repo)
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// TakeWorkspaceBundleForRestore answers the bundle issue's last release saved for the pod of
// podUID, created at podCreated, whose launcher it is about to be sent to, and records in the
// same statement what that ask did to the bundle. project is the token of the daemon whose pod
// asks (claim.ProjectToken). It answers runtime.WorkspaceBundleNone, having recorded nothing, when
// the issue has no bundle of project's — one never released, one whose restore the launcher
// confirmed, one whose release a wake cancelled, one that a pod's kept volume held
// (DropHeldWorkspaceBundle), one of an issue project does not record — when the row already
// carries an attempt, and when the pod asking is the pod the work came off (source_pod_uid).
//
// A bundle is project's only where project records its issue (issues.project, the Dispatch key
// the token lowercases). The row names no project, and its release's mark, which does, is gone
// once the release has finished, so the issue's record stands in for it. That holds because
// the one writer of a bundle is the save of an idle release (PutWorkspaceBundle, run by
// internal/daemon's bundleIssueWorkspace from the release's save row). A daemon's outbox runs
// only the rows of its own project's keys (record's DueRows), and a release only marks a pod
// whose issue its project records (ReleaseIdleIssue, readIdleView). So a bundle of an issue this
// project does not record was saved by another project's daemon on a database the two share,
// for that daemon's own pod. The operator's spawn makes a claim of such an issue under this
// project's token, and a new pod of that claim would otherwise spend the other daemon's one
// attempt and leave that daemon's pod nothing to restore.
//
// One attempt per bundle is the rule, and this statement is where it is kept. The launcher's own
// record of the restore it ran lives in the process that ran it: a roles container that restarts
// on the pod's own disk comes back with that record gone and the workspace as the roles left it,
// hours of work past the save, and a bundle sent again there would `jj edit` back to the saved
// head and move each bookmark `--allow-backwards` over all of it. The attempt is written before
// any slice leaves the daemon, so a send that fails halfway, a restore that fails and a restore
// whose report never arrived all leave the same row: one no pod is sent again.
//
// Whose attempt it is, the row's two pod columns decide, and they are the other half of the same
// rule. Every start of every role takes the bundle for its pod (sandbox's launchers.start), and
// a release stops the roles of the pod it is releasing: a role relaunched in that pod, between
// the save and the teardown that deletes the pod, is such a start. source_pod_uid is that pod,
// as the save recorded it (PutWorkspaceBundle): its workspace is the one the work was taken
// from, so it is answered nothing and the row is left as it was, and the attempt is still there
// for the pod that replaces it. Every other pod's ask takes the attempt, and the first of them
// is the only pod this bundle is ever offered to.
//
// What that pod is answered is podCreated's rule. A pod created after the save is the pod the
// teardown's launch made, and it is sent the work (runtime.WorkspaceBundleSend). A pod that is
// not is refused (runtime.WorkspaceBundleRefusedOlder) and the refusal closes the bundle: such a
// pod's workspace holds what its own fetch provisioned and whatever its roles have done since,
// and the pod that comes after it holds that work's descendants, so a bundle kept open would be
// applied over work pushed after the save. The bytes stay for a person to recover.
//
// Before either of those the row's own repository is read, since a pod's age decides nothing
// about work of a repository the issue is no longer on. The row names the repository its work
// is of (PutWorkspaceBundle) and the issues table names the one the issue is on now, which a
// correction of Dispatch's fact moves (record.Store.CorrectIssueRepository). Whether the issue
// has left the row's repository is left's to say, and the statement records the answer it is
// given: left is the one rule the daemon reads every such pair by (ghrepo.Renames.Left, through
// the daemon's repositories.left), which compares names without regard to case, as GitHub
// does, reads a configured rename's two names as one repository, and reads an empty name as
// unknown. The pairs are the daemon's configuration and the database holds none of them, so the
// decision is made here in Go and not in the statement. Where left answers that the issue has
// left the repository, the bundle is refused (runtime.WorkspaceBundleRefusedRepository) and
// nothing is sent. The pod's workspace is a clone of the issue's repository now and holds none
// of the saved commits' bases, so the bundle could not be applied there at all, and the daemon
// writes on the issue where the work is instead (internal/daemon's bundleRepositoryMessage). A
// row whose repo is empty — one saved before the column — and a record that names no repository
// are each read as an empty name, so neither refuses anything: a name nothing recorded is never a
// statement that the issue has left anything. An issue project does not record is answered
// nothing before any of this (the paragraph on whose bundle it is, above).
//
// A refusal for the repository moves the row out of workspace_bundles and into
// workspace_bundles_refused, in the transaction that records it. workspace_bundles holds one
// row per issue, and the issue's next release replaces that row with its own save, or deletes it
// when it saves nothing (the daemon's outbox.bundleIssueWorkspace); the end of the issue's work
// deletes it as well (dropReleasedWork). The issue is told that the daemon keeps this work, so
// the work cannot stay where any of those reaches it. The table it goes to has a row per
// refusal, which nothing in the daemon reads, changes or deletes again: an operator reads it by
// issue and release_id, which is what the message names, and deletes the row once the work is
// carried out. The row also stops holding the issue's pod by that move, since the idle rule
// reads workspace_bundles alone (workspaceBundleUnrestored).
//
// The comparison rests on two instants from two clocks: podCreated is the pod's creationTimestamp
// as the Kubernetes API server stamped it, and the row's `at` is when the shim took the bundle,
// by the clock of the node the released pod ran on (runtime.WorkspaceBundle.At). It cannot tell
// them apart more finely than the coarser of the two: a creationTimestamp has one-second
// resolution, so the save is read to its own second and a pod created inside that second is
// taken as the newer one. Nor can it see clock skew: a node running ahead of the API server
// stamps the save in the replacement pod's future, and that pod is refused. Refusing costs the
// restore and loses no work, which is the direction to fail in — the opposite, a restore applied
// over work done since, is the harm — and the refusal is recorded so that it costs that restore
// once rather than moving the issue's bookmarks back in the pod after it.
//
// The row stays for every other refusal. A bundle nothing restored holds the issue's pod
// (workflow.IdleHoldBundleUnrestored) and keeps the work in the database until the issue's own
// work is over (outbox's dropReleasedWork), which is what the launcher's notice tells the roles.
func (s *Store) TakeWorkspaceBundleForRestore(ctx context.Context, project, issue, podUID string, podCreated time.Time, left func(saved, recorded string) bool) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
	if err := restoreAsk.check(project, issue, podUID, podCreated); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, err
	}
	var b runtime.WorkspaceBundle
	var outcome string
	var tips, bookmarks []byte
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		// The lock is taken here, before left is asked, so the row the update below records the
		// answer on is the row whose repository was read, and a second pod asking at once waits
		// and then finds the attempt taken.
		offer, err := offeredBundle(ctx, tx, project, issue, podUID, podCreated)
		if err != nil {
			return err
		}
		outcome = "refused_older"
		switch {
		case left(offer.repo, offer.recorded):
			outcome = "refused_repository"
		case offer.savedBy:
			outcome = "sent"
		}
		// The body is read back only when it is being sent: a refusal records the attempt and
		// answers the row's facts alone, and a bundle runs to tens of megabytes.
		if err := tx.QueryRow(ctx, `update workspace_bundles
			set attempt_pod_uid = $2, attempt_at = now(), attempt_outcome = $3
			where issue = $1
			returning case when attempt_outcome = 'sent' then bundle end,
				at, head, tips, bookmarks, release_id, source_pod_uid, repo`,
			issue, podUID, outcome).Scan(&b.Bundle, &b.At, &b.Head, &tips, &bookmarks, &b.Release, &b.PodUID, &b.Repo); err != nil {
			return err
		}
		if outcome != "refused_repository" {
			return nil
		}
		// offeredBundle holds the row's lock, so the row this deletes is the one it refused.
		_, err = tx.Exec(ctx, `with refused as (delete from workspace_bundles where issue = $1
				returning issue, release_id, source_pod_uid, repo, bundle, size_bytes, at, head, tips, bookmarks, attempt_pod_uid, attempt_at)
			insert into workspace_bundles_refused (issue, release_id, source_pod_uid, repo, bundle, size_bytes, at, head, tips, bookmarks, refused_pod_uid, refused_at)
			select * from refused`, issue)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, nil
	}
	if err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("take the workspace bundle of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the workspace bundle of %s: its tips: %w", issue, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the workspace bundle of %s: its bookmarks: %w", issue, err)
	}
	// The outcome the statement wrote is the answer: `sent` for the body above, and one of the
	// two refusals for the pod it refused, whose attempt is spent all the same.
	switch outcome {
	case "refused_repository":
		return b, runtime.WorkspaceBundleRefusedRepository, nil
	case "sent":
		return b, runtime.WorkspaceBundleSend, nil
	default:
		return b, runtime.WorkspaceBundleRefusedOlder, nil
	}
}

// WorkspaceBundleSize is the size in bytes of the bundle issue's last release saved, and false
// when the issue has none. The release's teardown wants the size alone, and a bundle runs to
// tens of megabytes: reading the row's size_bytes rather than its bundle keeps those bytes out
// of the pool and out of the daemon's memory.
func (s *Store) WorkspaceBundleSize(ctx context.Context, issue string) (int, bool, error) {
	var size int
	err := s.pool.QueryRow(ctx, `select size_bytes from workspace_bundles where issue = $1`, issue).Scan(&size)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read the size of the workspace bundle of %s: %w", issue, err)
	}
	return size, true, nil
}

// DeleteWorkspaceBundle drops issue's bundle whatever release saved it: the release saved a
// workspace with nothing unpushed, or the issue's work is over. A cancelled release deletes its
// own bundle in its cancel's transaction (CancelIssueRelease), not here. Deleting one the table
// does not hold is success, so a retried delete finishes. The issue's bundle refusal goes in the
// same statement (RecordBundleRefusal), since either reason leaves no work for one to be about.
func (s *Store) DeleteWorkspaceBundle(ctx context.Context, issue string) error {
	if _, err := s.pool.Exec(ctx, `with ended as (delete from workspace_bundle_refusals where issue = $1)
		delete from workspace_bundles where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the workspace bundle of %s: %w", issue, err)
	}
	return nil
}

// DeleteRestoredWorkspaceBundle drops the bundle of release, which a pod's launcher has reported
// restored. It is the restore's own delete and names the release because a launcher holds its
// report until some connection carries it (internal/launcher's bundleRestore.owed): a report can
// reach the daemon after the issue's next release has saved a bundle of its own, and deleting the
// issue's row on it would take work no pod holds any more. The row goes only while it is still
// the one that release saved; a report of any other release deletes nothing, and its work stays
// for the issue's next pod. Deleting a row the table no longer holds is success, so a
// confirmation the daemon read twice finishes.
func (s *Store) DeleteRestoredWorkspaceBundle(ctx context.Context, issue string, release int64) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_bundles where issue = $1 and release_id = $2`, issue, release); err != nil {
		return fmt.Errorf("delete the restored workspace bundle of %s saved by release %d: %w", issue, release, err)
	}
	return nil
}

// DropHeldWorkspaceBundle deletes the bundle issue's last release saved where the pod of podUID
// holds that work already: the pod's Sandbox owns the issue's kept volume and was created at
// keptSince, before the save, so the pod mounts the volume the work was taken off. project is the
// token of the daemon whose pod asks (claim.ProjectToken). It answers the release and the time of
// the bundle it deleted, and true. It answers false, having deleted nothing, when the issue has no
// bundle of project's, when the bundle had its one attempt already, when the pod is the pod the
// work came off (source_pod_uid), when the Sandbox was not created before the save's own second,
// and when the issue has left the repository the work is of.
//
// It deletes the row a confirmed restore deletes (DeleteRestoredWorkspaceBundle), under the take's
// own condition (offeredBundle), and it reads none of the body: a bundle runs to tens of
// megabytes, and the volume holds the work. The take's three rules stand. The pod the work
// came off is a role relaunched in the pod being released, and the attempt stays for the pod that
// replaces it. A row whose attempt is spent is the record of that attempt, and it stays. A bundle
// is project's only where project records its issue (issues.project), with the take's own test of
// that record. A bundle of an issue that project does not record was saved by another project's
// daemon for that daemon's own pod, and it stays for that pod.
//
// The comparison reads the save to its own second, as the take does, and drops the bundle only
// when that second is after keptSince. A creationTimestamp has one-second resolution, so a Sandbox
// created in the save's own second cannot be ordered against the save, and the bundle stays for
// the take. That is the side to fail on: a drop deletes the only copy of the work, and a restore
// the launcher refuses costs a notice and holds the issue's pod. keptSince is the API server's
// clock and the save's time the clock of the released pod's node. A release comes at least the
// idle period after the Sandbox's first pod, and a reset makes its Sandbox after a later pod's
// init container failed, so a skew of seconds does not change the answer.
//
// The take's repository rule stands too, and left decides it as it decides the take's. A bundle
// of a repository the issue has left is kept for the take, which refuses it for its repository
// and moves it to workspace_bundles_refused, and the daemon writes on the issue where the work is.
// The kept volume holds that work, but in the workspace of the repository the issue left: a
// workspace's directory names its repository (workspace.Location), so the pod of the issue's
// repository now works in another workspace on the same volume, and the volume goes at a reset
// or at the issue's close. So the volume is no reason to delete that bundle. A row with no
// repository, saved before the column, and a record that names none, are each an empty name,
// which left never reads as a change, so the drop deletes such a row as it deletes any other.
//
// The drop refuses an empty project, an empty podUID and a zero keptSince, as the take refuses its
// own (bundleAsk.check).
func (s *Store) DropHeldWorkspaceBundle(ctx context.Context, project, issue, podUID string, keptSince time.Time, left func(saved, recorded string) bool) (runtime.WorkspaceBundle, bool, error) {
	if err := dropAsk.check(project, issue, podUID, keptSince); err != nil {
		return runtime.WorkspaceBundle{}, false, err
	}
	var held runtime.WorkspaceBundle
	var dropped bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		held, dropped = runtime.WorkspaceBundle{}, false
		offer, err := offeredBundle(ctx, tx, project, issue, podUID, keptSince)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if offer.savedBy || left(offer.repo, offer.recorded) {
			return nil
		}
		// offeredBundle holds the row's lock, so the row this deletes is the one it read.
		if err := tx.QueryRow(ctx, `delete from workspace_bundles where issue = $1 returning release_id, at`,
			issue).Scan(&held.Release, &held.At); err != nil {
			return err
		}
		dropped = true
		return nil
	})
	if err != nil {
		return runtime.WorkspaceBundle{}, false, fmt.Errorf("drop the workspace bundle of %s that the kept volume of the pod %s holds: %w", issue, podUID, err)
	}
	return held, dropped, nil
}

// bundleOffer is what offeredBundle reads of the bundle row that the take would offer a pod.
type bundleOffer struct {
	// repo is the repository the saved work is of (workspace_bundles.repo), and recorded is the
	// one the issue's record names now. Each is "" where nothing recorded a repository.
	repo, recorded string
	// savedBy is whether the save's own second is no later than the time the ask compares it
	// with. A pod created then is read as newer than the save. A kept Sandbox created then is not
	// read as older than the save, so it is not known to hold the volume the save was taken off.
	savedBy bool
}

// offeredBundle locks issue's bundle row that the take keeps its one attempt for
// (TakeWorkspaceBundleForRestore), and reads it as a bundleOffer, with at the time that the ask
// compares the save with. The row is the issue's, with no attempt yet, that did not come off the
// pod podUID, of an issue that project records. The take and the drop (DropHeldWorkspaceBundle)
// each read the row here, so the drop deletes only a row that the take would offer. It answers
// pgx.ErrNoRows, and locks nothing, where there is no such row. The lock holds to the end of tx,
// so the statement that records the ask's answer writes the row that was read, and a second pod
// that asks at once waits and then finds the attempt taken or the row gone.
//
// The save is read to its own second (date_trunc), because the time it is compared with is one
// the API server stamped, and a creationTimestamp has one-second resolution.
func offeredBundle(ctx context.Context, tx pgx.Tx, project, issue, podUID string, at time.Time) (bundleOffer, error) {
	var offer bundleOffer
	err := tx.QueryRow(ctx, `select repo, coalesce((select i.repo from issues i where i.key = $1), ''),
			date_trunc('second', at) <= $3
		from workspace_bundles
		where issue = $1 and attempt_pod_uid is null and source_pod_uid <> $2
			and exists (select 1 from issues i where i.key = $1 and lower(i.project) = $4)
		for update`,
		issue, podUID, at, project).Scan(&offer.repo, &offer.recorded, &offer.savedBy)
	return offer, err
}

// bundleAsk names one ask of an issue's workspace bundle in the refusals of its arguments (check).
// what names the ask, with a %s for the issue. pod names the pod of the ask. when names the time
// that the ask compares with the save, with a %s for the pod.
type bundleAsk struct{ what, pod, when string }

// The two asks of a bundle that a pod makes: the take of its one restore attempt, and the drop
// of a bundle that the pod's kept volume holds.
var (
	restoreAsk = bundleAsk{what: "the restore attempt of the workspace bundle of %s",
		pod: "the pod it is sent to", when: "when the pod %s it is sent to was created"}
	dropAsk = bundleAsk{what: "the drop of the workspace bundle of %s that a kept volume holds",
		pod: "the pod that mounts it", when: "when the Sandbox of the pod %s was created"}
)

// check refuses an ask of issue's bundle that misses one of the three arguments that the take and
// the drop each need: the project whose pod asks, the pod podUID, and the time at. An empty podUID
// would never be the pod that the work came off, and a zero at would read as older than every
// save.
func (a bundleAsk) check(project, issue, podUID string, at time.Time) error {
	switch {
	case project == "":
		return fmt.Errorf(a.what+" needs the project whose pod asks", issue)
	case podUID == "":
		return fmt.Errorf(a.what+" needs "+a.pod, issue)
	case at.IsZero():
		return fmt.Errorf(a.what+" needs "+a.when, issue, podUID)
	}
	return nil
}

// workspaceBundleUnrestored is whether issue has a stored bundle whose restore is not recorded,
// which is the idle rule's reason to release no pod of the issue (store.readIdleView,
// workflow.IdleHoldBundleUnrestored). The row's own presence is that record: a restore the pod's
// launcher confirmed deletes it (DeleteRestoredWorkspaceBundle), and so does the drop for a pod
// whose kept volume holds the work (DropHeldWorkspaceBundle). Each does so before any role of
// the pod starts, so a row still here when the idle rule reads it is work no pod has put back —
// the release that would overwrite it is the release the hold stops. A bundle whose one attempt
// failed is such a row, and so is one whose attempt was spent refusing a pod no newer than the
// save (attempt_outcome, TakeWorkspaceBundleForRestore); each holds the issue's pod until the
// issue's work is over, and the row's own columns say which it was. The work is in the database
// alone, and a release of that pod would take the disk the roles have worked on since.
//
// A bundle refused for its repository is not read here, because it is not in this table: the
// refusal moved it to workspace_bundles_refused (TakeWorkspaceBundleForRestore), where no
// release of the issue can overwrite or delete it. So it holds no pod. A hold for it would have no
// end, since no restore is ever recorded for a refused bundle, and the work does not need one.
func workspaceBundleUnrestored(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var unrestored bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from workspace_bundles where issue = $1)`, issue).Scan(&unrestored); err != nil {
		return false, fmt.Errorf("read whether the workspace bundle of %s is unrestored: %w", issue, err)
	}
	return unrestored, nil
}

// RecordBundleRefusal records that the save of issue's release found the workspace's unpushed
// work over the bound a release carries (workflow.IdleHoldBundleTooLarge). It keeps the moment
// that release was marked and the head of the issue's pull request now, which the idle rule
// reads (workspaceBundleRefusal, workflow.IdleRelease) to hold the pod until the rule
// workflow.IdleHoldBundleTooLarge states ends the hold. The moment is the mark's and not the
// refusal's, so a turn that ran while the bundle was built counts as a turn after it. The caller
// writes this before it cancels the release, so a cancel that fails and runs again finds the
// refusal already recorded, and a daemon restart reads it. A save that failed for a reason of its
// own records nothing here: no push or turn cures it, so its release is marked again on the next
// tick.
//
// tell is the rows that tell the issue's worker about the refusal, encoded by the caller, as
// every row the store queues for the outbox is (IdleReleaseRow). They are queued in the same
// transaction, and only when the issue's last refusal was at another head, or there was none:
// the worker is told once for each head the work was refused at, however many saves are refused
// at it. It answers whether they were queued.
func (s *Store) RecordBundleRefusal(ctx context.Context, project, issue string, tell []IdleReleaseRow) (bool, error) {
	if project == "" || issue == "" {
		return false, errors.New("a bundle refusal needs its project and its issue")
	}
	var told bool
	err := s.Tx(ctx, func(tx pgx.Tx) error {
		told = false
		var head string
		if err := tx.QueryRow(ctx, `select coalesce((select head_sha from pull_requests where issue = $1), '')`,
			issue).Scan(&head); err != nil {
			return fmt.Errorf("read the head of %s's pull request: %w", issue, err)
		}
		var earlierHead string
		err := tx.QueryRow(ctx, `select head from workspace_bundle_refusals where issue = $1 for update`,
			issue).Scan(&earlierHead)
		known := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("read the last bundle refusal of %s: %w", issue, err)
		}
		if _, err := tx.Exec(ctx, `insert into workspace_bundle_refusals (issue, at, head)
			values ($1, coalesce((select marked_at from issue_releases where project = $2 and issue = $1), now()), $3)
			on conflict (issue) do update set at = excluded.at, head = excluded.head`,
			issue, project, head); err != nil {
			return fmt.Errorf("record the bundle refusal of %s: %w", issue, err)
		}
		if known && earlierHead == head {
			return nil
		}
		for _, row := range tell {
			if _, err := insertOutboxRow(ctx, tx, project, issue, row); err != nil {
				return err
			}
		}
		told = len(tell) > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return told, nil
}

// workspaceBundleRefusal is issue's last bundle refusal (RecordBundleRefusal), nil when there is
// none, and the head of the issue's pull request now, read beside it for the idle rule's view
// (readIdleView, pods.IdleView.BundleRefusal). The head is "" with no pull request, as the
// refusal records it, so the two compare as workflow.IdleHoldBundleTooLarge states.
func workspaceBundleRefusal(ctx context.Context, tx pgx.Tx, issue string) (*pods.BundleRefusal, string, error) {
	var refusal pods.BundleRefusal
	var head string
	err := tx.QueryRow(ctx, `select r.at, r.head, coalesce(p.head_sha, '')
		from workspace_bundle_refusals r left join pull_requests p on p.issue = r.issue
		where r.issue = $1`, issue).Scan(&refusal.At, &refusal.Head, &head)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("read the bundle refusal of %s: %w", issue, err)
	}
	return &refusal, head, nil
}

// bundleJSON renders the two members the row keeps as jsonb. Each is written even when empty — a
// bundle with no local bookmark is `{}`, not null — so every row reads back the same way.
func bundleJSON(b runtime.WorkspaceBundle) (tips, bookmarks []byte, err error) {
	if b.Tips == nil {
		b.Tips = []string{}
	}
	if b.Bookmarks == nil {
		b.Bookmarks = map[string]string{}
	}
	if tips, err = json.Marshal(b.Tips); err != nil {
		return nil, nil, fmt.Errorf("its tips: %w", err)
	}
	if bookmarks, err = json.Marshal(b.Bookmarks); err != nil {
		return nil, nil, fmt.Errorf("its bookmarks: %w", err)
	}
	return tips, bookmarks, nil
}
