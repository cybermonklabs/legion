package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// PutWorkspaceBundle saves issue's workspace bundle as the work of release, replacing whatever the
// issue's last release left: one issue has one bundle, the newest release's, and a release that
// ran again after a wake cancelled the first must not leave the older work behind to be restored
// instead. release is the id of the idle release making the save (issue_releases.id), which the
// restore of the issue's next pod carries to that pod's launcher and back, so the report that
// confirms a restore names the release whose work it put back (KeepRestoredWorkspaceBundle).
// The save clears the restore attempt the row carried with the rest of it: the work of this
// release is another bundle, and it gets an attempt of its own (TakeWorkspaceCopyForRestore).
//
// The bundle names the pod its work came off (runtime.WorkspaceBundle.PodUID), which the row
// keeps as source_pod_uid and the take refuses by: that pod's own workspace is where the work
// is, so a role relaunched in it must be answered nothing and must leave the attempt for the pod
// that replaces it. A save with no such pod is refused rather than stored, since the row could
// not then tell the released pod from its replacement.
//
// The issue's running copy is dropped in the save's own transaction, whatever minute took it
// (DeleteRunningWorkspaceCopy). This bundle is the newer copy of the same workspace — it is taken
// off the disk that copy came off, while the roles are idle and just before the release stops
// them — so from this moment the running copy is stale, and leaving it would mean a pod after the
// next one being sent work older than the bundle the next one restored. The two are one write
// because a save that stored the bundle and then failed to drop the copy would have left the very
// copy this rule exists to remove. The bundle is that pod's last copy from here on: no running
// copy off it is stored after the save (PutRunningWorkspaceCopy, releaseBundleSavedOff).
func (s *Store) PutWorkspaceBundle(ctx context.Context, issue string, release int64, b runtime.WorkspaceBundle) error {
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
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `insert into workspace_bundles (issue, release_id, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
			values ($1, $2, $3, $4, $5, $6, $7, $8, $9)
			on conflict (issue) do update set release_id = excluded.release_id, source_pod_uid = excluded.source_pod_uid,
				bundle = excluded.bundle, size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
				tips = excluded.tips, bookmarks = excluded.bookmarks,
				attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`,
			issue, release, b.PodUID, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks); err != nil {
			return err
		}
		return deleteRunningCopy(ctx, tx, issue)
	})
	if err != nil {
		return fmt.Errorf("save the workspace bundle of %s: %w", issue, err)
	}
	return nil
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
// does not hold is success, so a retried delete finishes.
//
// The issue's running copy goes in the same transaction, whatever minute took it, because each
// reason to drop the bundle is a reason to drop that copy as firmly. A release that found nothing
// unpushed says there is no unpushed work to put back at all, so a copy of a few minutes ago is
// work that has since been pushed, and restoring it would move the issue's bookmarks back over
// it; and an issue whose work is over has nothing left for any pod to restore. A delete of the
// bundle that left the copy behind would leave exactly that copy for the issue's next pod — a
// later generation's, should the issue be reopened.
func (s *Store) DeleteWorkspaceBundle(ctx context.Context, issue string) error {
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `delete from workspace_bundles where issue = $1`, issue); err != nil {
			return err
		}
		return deleteRunningCopy(ctx, tx, issue)
	})
	if err != nil {
		return fmt.Errorf("delete the workspace bundle and the running copy of %s: %w", issue, err)
	}
	return nil
}

// KeepRestoredWorkspaceBundle keeps the bundle of release, which the launcher of the pod of podUID
// has reported restored, as that pod's own copy of the issue's work. In one transaction it deletes
// the bundle's row, which lifts the hold the row puts on the issue's pod
// (workspaceBundleUnrestored, workflow.IdleHoldBundleUnrestored), and writes the same work as the
// issue's running copy, sourced to the pod that restored it, under a restore id of its own and
// with no attempt spent. The bytes move inside the database and never through the daemon.
//
// The work is kept because from the confirmation on it is on the restoring pod's disk alone, and
// none of the three triggers of a running copy need fire for a pod that does nothing: a relaunched
// agent comes up Ready rather than ending a turn, the cadence copies only an issue in a turn, and
// the push copy needs a push. Deleted, the work would go with that pod should the cluster end it
// again before its first turn. Kept as the pod's own copy, it is answered nothing to that pod,
// whose workspace already holds it (the take's source rule, runtime.DecideWorkspaceRestore), and
// is offered to the issue's next pod. It holds no pod, as no running copy does, and it goes the
// way every running copy goes: the pod's own copies replace it as they are taken, an idle release
// of that pod drops it in the save's own transaction (PutWorkspaceBundle), and a workspace that
// bundles to nothing drops it (DeleteRunningWorkspaceCopy). The release guard is not touched: the
// bundle's row is gone, so no copy is refused for having come off the pod it was saved off
// (releaseBundleSavedOff).
//
// The kept copy is stamped with the confirming transaction's now() rather than the bundle's own
// time. The row stands for the restoring pod's workspace as it is once the restore is confirmed,
// which is when that pod's copy of the work begins. A release bundle is exempt from the push rule
// and a running copy is not: under the bundle's time, a push the daemon recorded after the save —
// an outside push during the release, or one after the restore — would refuse the kept copy to the
// next pod (runtime.WorkspaceBundleRefusedPushed) and lose the work again. A push recorded after
// the confirmation takes a copy of its own off the restoring pod's live roles (internal/daemon's
// copyWorkspaceAtPush), so the rule keeps its meaning. The times compared are then the database's
// clock against the API server's, for the pod the copy is offered to, and against the daemon's,
// for a push.
//
// A running copy the issue already holds is replaced only where the bundle's work is the newer of
// the two by the restore's own rule (runtime.DecideWorkspaceRestore), read with the bundle's own
// time, because the newer work wins. The decision that sent this bundle superseded such a copy,
// and no pod is sent a superseded copy again, so replacing it loses nothing a pod would restore. A
// copy the rule reads as the newer — one taken off another pod after the bundle — stays as it is,
// and the bundle's row goes all the same, since the restore it confirms is done.
//
// It names the release because a launcher holds its report until some connection carries it
// (internal/launcher's bundleRestore.owed): a report can reach the daemon after the issue's next
// release has saved a bundle of its own, and that bundle is work no pod holds any more. A report of
// any release but the one whose row the issue holds changes nothing, and so does a confirmation the
// daemon read twice, the row being gone. A report with no pod to name is refused, since the kept
// row could not then tell the pod the work is on from the pod that replaces it.
func (s *Store) KeepRestoredWorkspaceBundle(ctx context.Context, issue, podUID string, release int64) error {
	if podUID == "" {
		return fmt.Errorf("the restored workspace bundle of %s saved by release %d needs the pod that restored it", issue, release)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Both rows are locked in the order the restore's own take locks them
		// (TakeWorkspaceCopyForRestore), the bundle first.
		var bundle runtime.WorkspaceCopyHeld
		err := tx.QueryRow(ctx, `select at, source_pod_uid from workspace_bundles
			where issue = $1 and release_id = $2 for update`, issue, release).Scan(&bundle.At, &bundle.Source)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		running, err := heldCopy(ctx, tx, runningCopyCopies, issue)
		if err != nil {
			return err
		}
		// Which copy is the newer is a fact of the two copies alone, so the push and the pod the
		// decision is otherwise asked for change nothing of it.
		if _, _, newer := runtime.DecideWorkspaceRestore(bundle, running, runtime.WorkspacePush{}, "", time.Time{}); newer == runtime.WorkspaceCopyRunning {
			_, err := tx.Exec(ctx, `delete from workspace_bundles where issue = $1 and release_id = $2`, issue, release)
			return err
		}
		_, err = tx.Exec(ctx, `with kept as (
				delete from workspace_bundles where issue = $1 and release_id = $2
				returning bundle, size_bytes, head, tips, bookmarks)
			insert into workspace_running_copies (issue, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
			select $1, $3, bundle, size_bytes, now(), head, tips, bookmarks from kept
			on conflict (issue) do update set restore_id = nextval('workspace_running_copy_restore_ids'),
				source_pod_uid = excluded.source_pod_uid, bundle = excluded.bundle,
				size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
				tips = excluded.tips, bookmarks = excluded.bookmarks,
				attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`, issue, release, podUID)
		return err
	})
	if err != nil {
		return fmt.Errorf("keep the restored workspace bundle of %s saved by release %d as the copy of pod %s: %w", issue, release, podUID, err)
	}
	return nil
}

// workspaceBundleUnrestored is whether issue has a stored bundle whose restore is not recorded,
// which is the idle rule's reason to release no pod of the issue (store.readIdleView,
// workflow.IdleHoldBundleUnrestored). The row's own presence is that record: a restore the pod's
// launcher confirmed deletes it (KeepRestoredWorkspaceBundle), and it does so before any role of
// the pod starts, so a row still here when the idle rule reads it is work no pod has put back —
// the release that would overwrite it is the release the hold stops. A bundle whose one attempt
// failed is such a row, and so is one whose attempt was spent refusing it to a pod or closing it
// unsent (attempt_outcome, TakeWorkspaceCopyForRestore); each holds the issue's pod until the
// issue's work is over, and the row's own columns say which it was. The work is in the database
// alone, and a release of that pod would take the disk the roles have worked on since.
func workspaceBundleUnrestored(ctx context.Context, tx pgx.Tx, issue string) (bool, error) {
	var unrestored bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from workspace_bundles where issue = $1)`, issue).Scan(&unrestored); err != nil {
		return false, fmt.Errorf("read whether the workspace bundle of %s is unrestored: %w", issue, err)
	}
	return unrestored, nil
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
