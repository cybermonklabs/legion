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

// MinRunningWorkspaceCopyRestoreID is where the restore ids of running copies begin, 2^62, and
// every id issue_releases hands a release bundle is below it: the migration that made this table
// holds issue_releases.id under the same number, so the two spaces are disjoint by the schema
// (migrations' 20261112105448_workspace_running_copies).
//
// The boundary is what a late restore report is routed on. A launcher holds its report until some
// connection carries it, so one can reach a daemon that has restarted since the send, and the id
// is all it carries: an id below this one names the release bundle of that release
// (DeleteRestoredWorkspaceBundle), an id at or above it names a running copy
// (DeleteRestoredRunningWorkspaceCopy). The space is positive because the launcher refuses a
// bundle slice whose release is not (shimwire.LauncherBundle.Validate), so the kind cannot be
// marked by a sign.
const MinRunningWorkspaceCopyRestoreID int64 = 1 << 62

// PutRunningWorkspaceCopy saves the unpushed work of issue's workspace as the copy taken while its
// pod runs, replacing whatever the issue's last copy left: one issue has one running copy, the
// newest, and a copy of an older minute must never be restored in place of a newer one. The copy
// is written to a table of its own and never to the row an idle release saves, because the mere
// presence of that row holds the issue's pod (workspaceBundleUnrestored,
// workflow.IdleHoldBundleUnrestored) and a copy of a live disk's work would hold every working
// issue's pod for ever, as well as destroying a release bundle whose restore no pod has confirmed.
//
// Each copy the save stores takes a restore id of its own and clears the restore attempt the row
// carried with the rest of it: it is other work than the copy it replaces, so it is offered to a
// pod of its own (TakeWorkspaceCopyForRestore), and a report naming the id it replaced
// deletes nothing.
//
// A save whose content equals the stored row's moves the time alone and leaves the bytes, the size
// and the restore id where they are. Equal means the same head, the same tips, the same bookmarks
// and the same size, taken off the same pod. A turn that reads the workspace and writes nothing in
// it ends with the work the copy before it already holds, and rewriting a multi-megabyte row every
// five minutes to store the bytes again is waste, which is a design defect (AGENTS.md): the row
// that is already there is the same copy, so the save only records that the copy is current as of
// a later minute. Its spent attempt stays spent with the rest of it — nothing about the work has
// changed, so the pod that was already offered it is still the pod this copy was offered to — and
// the first copy whose content differs is a new row with a new id and an attempt of its own. The
// pod is part of the comparison because the rest of the row is read against it: a workspace that
// comes back unchanged on a pod that replaced the first would otherwise keep naming the pod that
// is gone as its source, and the take would then answer the copy to the pod it was taken off.
//
// A copy with no pod to name as its source is refused rather than stored, since the row could not
// then tell the pod the work is on from the pod that replaces it, and so is one carrying no bytes:
// a bundle of nothing restores nothing, and a row holding one would be answered to a pod in place
// of the release bundle it should have had.
func (s *Store) PutRunningWorkspaceCopy(ctx context.Context, issue string, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a running workspace copy needs its issue")
	}
	if b.PodUID == "" {
		return fmt.Errorf("the running workspace copy of %s needs the uid of the pod its work came off", issue)
	}
	if len(b.Bundle) == 0 {
		return fmt.Errorf("the running workspace copy of %s carries no work", issue)
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	// The unchanged workspace is answered first and without its bytes, so the common copy of a
	// turn that changed nothing neither carries tens of megabytes to the database nor writes
	// them again. A workspace that did change matches no row here and is written whole below,
	// at the cost of one indexed read that touched nothing.
	moved, err := s.pool.Exec(ctx, `update workspace_running_copies set at = $2
		where issue = $1 and source_pod_uid = $3 and head = $4 and size_bytes = $5
			and tips = $6 and bookmarks = $7`,
		issue, b.At, b.PodUID, b.Head, b.Size(), tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	if moved.RowsAffected() > 0 {
		return nil
	}
	_, err = s.pool.Exec(ctx, `insert into workspace_running_copies
			(issue, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
		values ($1, $2, $3, $4, $5, $6, $7, $8)
		on conflict (issue) do update set restore_id = nextval('workspace_running_copy_restore_ids'),
			source_pod_uid = excluded.source_pod_uid, bundle = excluded.bundle,
			size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
			tips = excluded.tips, bookmarks = excluded.bookmarks,
			attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`,
		issue, b.PodUID, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// The two tables a saved copy of an issue's workspace is kept in, and the column each keeps the
// copy's restore id in, with the attempt_outcome each take that spends an attempt records. The
// restore's statements are written once for both tables and formatted with these constants and
// nothing else, so the read the decision is made from and the write that records it are one text
// whichever table a copy is in.
const (
	workspaceBundlesTable   = "workspace_bundles"
	workspaceBundlesID      = "release_id"
	runningCopiesTable      = "workspace_running_copies"
	runningCopiesRestoreID  = "restore_id"
	refusedPushedOutcome    = "refused_pushed"
	supersededCopyOutcome   = "superseded"
	sentCopyOutcome         = "sent"
	refusedOlderCopyOutcome = "refused_older"
)

// copyOutcomes is the attempt_outcome each take that spends an attempt records, and the one place
// a take and its outcome meet. A take it does not name fails the take (takeCopy) rather than
// record an outcome this file was not written for.
var copyOutcomes = map[runtime.WorkspaceBundleTake]string{
	runtime.WorkspaceBundleSend:          sentCopyOutcome,
	runtime.WorkspaceBundleRefusedOlder:  refusedOlderCopyOutcome,
	runtime.WorkspaceBundleRefusedPushed: refusedPushedOutcome,
	runtime.WorkspaceBundleSuperseded:    supersededCopyOutcome,
}

// copyTable is one of the two tables as the restore's statements name it.
type copyTable struct {
	kind  runtime.WorkspaceCopyKind
	table string
	id    string
}

var (
	releaseBundleCopies = copyTable{kind: runtime.WorkspaceCopyReleaseBundle, table: workspaceBundlesTable, id: workspaceBundlesID}
	runningCopyCopies   = copyTable{kind: runtime.WorkspaceCopyRunning, table: runningCopiesTable, id: runningCopiesRestoreID}
)

// TakeWorkspaceCopyForRestore answers what issue holds of its workspace for the pod of podUID,
// created at podCreated, whose launcher it is about to be sent to, and records what that ask did
// to each copy, all in one transaction: it reads the issue's release bundle, its running copy and
// the newest push of its branch the daemon recorded, decides between them
// (runtime.DecideWorkspaceRestore, where every rule of the choice is written: the push refuses a
// running copy taken before it and never a release bundle), and spends the attempt of every copy
// the decision answered anything but runtime.WorkspaceBundleNone, answering each copy's take in
// that copy's own field (runtime.WorkspaceRestore). It is one transaction because the choice is
// one decision: made as separate statements it could close the older copy and then fail to take
// the newer, leaving the store half-changed and the pod given neither. A failure anywhere in it
// leaves every row as it was.
//
// Both rows are read `for update`, so a save of either copy that lands while the decision is being
// made waits for it, and the copy the decision read is the copy whose attempt is spent. A copy is
// read by the facts the decision reads until it is taken; a copy runs to tens of megabytes, and
// only the one being sent is read with its body.
//
// The copy each answer carries names its restore id as its Release (runtime.WorkspaceBundle.Release),
// which the restore sends to the launcher and reads back out of its report: a release bundle's is
// the release that saved it, a running copy's an id above every release's
// (MinRunningWorkspaceCopyRestoreID), so a report that reaches the daemon after a restart still
// says which of the issue's two copies it confirms (DeleteRestoredWorkspaceCopy).
//
// A refused or superseded copy keeps its bytes for an operator. A release bundle nothing restored
// holds the issue's pod (workflow.IdleHoldBundleUnrestored) and keeps the work in the database
// until the issue's own work is over (outbox's dropReleasedWork), which is what the launcher's
// notice tells the roles. A running copy nothing restored holds no pod: the work it holds is on the
// disk it was taken off, so an idle release of that pod saves it again as a release bundle, and
// the idle rule reads workspace_bundles alone (workspaceBundleUnrestored).
func (s *Store) TakeWorkspaceCopyForRestore(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceRestore, error) {
	if podUID == "" {
		return runtime.WorkspaceRestore{}, fmt.Errorf("the restore attempt of the saved workspace copies of %s needs the pod it is sent to", issue)
	}
	if podCreated.IsZero() {
		return runtime.WorkspaceRestore{}, fmt.Errorf("the restore attempt of the saved workspace copies of %s needs when the pod %s it is sent to was created", issue, podUID)
	}
	var restore runtime.WorkspaceRestore
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		restore = runtime.WorkspaceRestore{}
		bundle, err := heldCopy(ctx, tx, releaseBundleCopies, issue)
		if err != nil {
			return err
		}
		running, err := heldCopy(ctx, tx, runningCopyCopies, issue)
		if err != nil {
			return err
		}
		// Nothing was released and no copy was taken, or every copy has been restored and
		// dropped: the pod starts on what its fetch provisioned, having spent nothing.
		if !bundle.Held() && !running.Held() {
			return nil
		}
		if restore.Push, err = issuePush(ctx, tx, issue); err != nil {
			return err
		}
		var bundleTake, runningTake runtime.WorkspaceBundleTake
		bundleTake, runningTake, restore.Offered = runtime.DecideWorkspaceRestore(bundle, running, restore.Push, podUID, podCreated)
		for _, decided := range []struct {
			copies copyTable
			take   runtime.WorkspaceBundleTake
			taken  *runtime.WorkspaceCopyTaken
		}{{releaseBundleCopies, bundleTake, &restore.ReleaseBundle}, {runningCopyCopies, runningTake, &restore.Running}} {
			if decided.take == runtime.WorkspaceBundleNone {
				continue
			}
			if *decided.taken, err = takeCopy(ctx, tx, decided.copies, issue, podUID, decided.take); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return runtime.WorkspaceRestore{}, fmt.Errorf("take the saved workspace copies of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	return restore, nil
}

// heldCopy is what issue's copy in copies' table says of itself to the restore's decision — when
// the shim took it, whether its one attempt is spent and the pod its work came off — and the zero
// value when the issue holds none, the row locked for the rest of the restore's transaction. Every
// row names its pod: source_pod_uid is `not null` in both tables (migrations'
// 20261103214558_workspace_bundle_pod_binding and 20261112105448_workspace_running_copies).
func heldCopy(ctx context.Context, tx pgx.Tx, copies copyTable, issue string) (runtime.WorkspaceCopyHeld, error) {
	var held runtime.WorkspaceCopyHeld
	err := tx.QueryRow(ctx, fmt.Sprintf(`select at, attempt_pod_uid is not null, source_pod_uid
		from %s where issue = $1 for update`, copies.table), issue).Scan(&held.At, &held.Spent, &held.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceCopyHeld{}, nil
	}
	if err != nil {
		return runtime.WorkspaceCopyHeld{}, fmt.Errorf("read the %s: %w", copies.kind, err)
	}
	return held, nil
}

// takeCopy spends the one attempt of issue's copy in copies' table on the pod of podUID, recording
// the take the decision answered for it (runtime.DecideWorkspaceRestore), and answers the row as
// the take read it back. The row was read for update in this same transaction (heldCopy), so a
// take that finds none is an error like any other. The body is read back only when it is being
// sent.
func takeCopy(ctx context.Context, tx pgx.Tx, copies copyTable, issue, podUID string, take runtime.WorkspaceBundleTake) (runtime.WorkspaceCopyTaken, error) {
	outcome, ok := copyOutcomes[take]
	if !ok {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("take the %s: the decision answered %v, which records no outcome", copies.kind, take)
	}
	taken := runtime.WorkspaceCopyTaken{Take: take}
	b := &taken.Bundle
	var tips, bookmarks []byte
	err := tx.QueryRow(ctx, fmt.Sprintf(`update %[1]s
		set attempt_pod_uid = $2, attempt_at = now(), attempt_outcome = $3
		where issue = $1
		returning case when attempt_outcome = '%[3]s' then bundle end, size_bytes,
			at, head, tips, bookmarks, %[2]s, source_pod_uid`, copies.table, copies.id, sentCopyOutcome),
		issue, podUID, outcome).
		Scan(&b.Bundle, &taken.Size, &b.At, &b.Head, &tips, &bookmarks, &b.Release, &b.PodUID)
	if err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("take the %s: %w", copies.kind, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("read the %s: its tips: %w", copies.kind, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("read the %s: its bookmarks: %w", copies.kind, err)
	}
	return taken, nil
}

// DeleteRunningWorkspaceCopy drops issue's running copy whatever minute took it. It is the
// cadence's own delete, of a workspace that bundled to nothing: a copy taken minutes before holds
// work that has since been pushed, and the next pod must never restore it. Every other path that
// invalidates what the issue holds drops the copy inside its own transaction, with the release
// bundle it saves or drops (PutWorkspaceBundle, DeleteWorkspaceBundle, CancelIssueRelease).
// Deleting one the table does not hold is success, so a retried delete finishes.
func (s *Store) DeleteRunningWorkspaceCopy(ctx context.Context, issue string) error {
	if err := deleteRunningCopy(ctx, s.pool, issue); err != nil {
		return fmt.Errorf("delete the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// deleteRunningCopy drops issue's running copy whatever minute took it, through db: the pool for
// the cadence's own delete (DeleteRunningWorkspaceCopy), and the transaction of every write that
// drops the copy with what it changes (PutWorkspaceBundle, DeleteWorkspaceBundle,
// CancelIssueRelease). Deleting one the table does not hold is success.
func deleteRunningCopy(ctx context.Context, db execer, issue string) error {
	_, err := db.Exec(ctx, `delete from workspace_running_copies where issue = $1`, issue)
	return err
}

// DeleteRestoredRunningWorkspaceCopy drops issue's running copy of restoreID, which a launcher of a
// pod of issue has reported restored. The id says which copy, because a launcher holds its report
// until some connection carries it, so one can reach the daemon after the issue's cadence has taken
// a copy of its own, and deleting the issue's row on the issue alone would take the newer work.
// Each save gives the copy it stores an id of its own (PutRunningWorkspaceCopy), so a report of a
// copy the issue has replaced deletes nothing and the newer copy stays for the issue's next pod.
// The issue bounds the id to the work of the launcher that reports it: the session the report
// came over is a pod of issue (the Secret its launcher authenticated with names the issue), and an
// id of another issue's copy deletes nothing, so a launcher of one issue cannot drop the work
// another issue's next pod would restore. An id below MinRunningWorkspaceCopyRestoreID is a
// release's and matches no row here, which is the disjointness the schema keeps. Deleting a row
// the table no longer holds is success, so a confirmation the daemon read twice finishes.
func (s *Store) DeleteRestoredRunningWorkspaceCopy(ctx context.Context, issue string, restoreID int64) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_running_copies where restore_id = $1 and issue = $2`, restoreID, issue); err != nil {
		return fmt.Errorf("delete the restored running workspace copy %d of %s: %w", restoreID, issue, err)
	}
	return nil
}

// DeleteRestoredWorkspaceCopy drops the copy of issue's workspace a pod's launcher reports it
// restored, routing on the restore id. That id is all such a report carries that is certain of
// the row: a launcher holds its report until some connection carries it (internal/launcher's
// bundleRestore.owed), so one can reach a daemon that has restarted since the send, with no
// memory of what it sent that pod.
//
// The id says which of the two tables the copy is in, because the two draw from disjoint spaces
// and the schema keeps them so (MinRunningWorkspaceCopyRestoreID). An id at or above the boundary
// is a running copy's, and that delete names the issue and the id together: the id because the
// issue's cadence may have taken a newer copy since and that row must stay for the issue's next
// pod, and the issue because the report is that issue's launcher's and another issue's copy is
// none of its business. An id below it is the id of the release whose teardown saved the bundle,
// and that delete names the issue and the release together, which is the same rule: a bundle a
// later release saved stays.
//
// The rule lives here, beside the two deletes it chooses between, because it is a statement about
// which table an id names and about nothing else: the daemon wires this method straight onto the
// runtime's report callback (runtime.WorkspaceBundleRestored), and the sandbox runtime's own
// tests drive the report through it rather than through a copy of the branch.
//
// An id of either kind that no row of issue holds deletes nothing, which is the ordinary end of a
// late report, and the caller is told the delete succeeded, because it did: the row the report
// confirmed is gone.
func (s *Store) DeleteRestoredWorkspaceCopy(ctx context.Context, issue string, restoreID int64) error {
	if restoreID >= MinRunningWorkspaceCopyRestoreID {
		return s.DeleteRestoredRunningWorkspaceCopy(ctx, issue, restoreID)
	}
	return s.DeleteRestoredWorkspaceBundle(ctx, issue, restoreID)
}
