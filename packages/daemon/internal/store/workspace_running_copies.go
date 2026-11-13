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
// copy's restore id in. The restore's statements are written once for both tables and formatted
// with these constants and nothing else, so the one-attempt rule and the clock rule are one text
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
// (runtime.DecideWorkspaceRestore), and spends the attempt of every copy the decision touched. It
// is one transaction because the choice is one decision: made as separate statements it could
// close the older copy and then fail to take the newer, leaving the store half-changed and the pod
// given neither. A failure anywhere in it leaves every row as it was.
//
// Both rows are read `for update`, so a save of either copy that lands while the decision is being
// made waits for it, and the copy the decision read is the copy whose attempt is spent. A copy is
// read by its `at` alone until it is taken; a copy runs to tens of megabytes, and only the one
// being sent is read with its body.
//
// The copy each answer carries names its restore id as its Release (runtime.WorkspaceBundle.Release),
// which the restore sends to the launcher and reads back out of its report: a release bundle's is
// the release that saved it, a running copy's an id above every release's
// (MinRunningWorkspaceCopyRestoreID), so a report that reaches the daemon after a restart still
// says which of the issue's two copies it confirms (DeleteRestoredWorkspaceCopy).
//
// What the decision does not settle, the statement that spends the attempt does (takeCopy):
//
//   - One attempt per copy. The launcher's own record of the restore it ran lives in the process
//     that ran it: a roles container that restarts on the pod's own disk comes back with that
//     record gone and the workspace as the roles left it, hours of work past the copy, and a copy
//     sent again there would `jj edit` back to its head and move each bookmark `--allow-backwards`
//     over all of it. The attempt is written before any slice leaves the daemon, so a send that
//     fails halfway, a restore that fails and a restore whose report never arrived all leave the
//     same row: one no pod is sent again.
//   - None spent by the pod the work came off (source_pod_uid). Every start of every role takes
//     for its pod (sandbox's launchers.start), and that pod goes on starting roles — a release
//     stops them and a role relaunched before the teardown starts again, and a cadence copies a pod
//     that is working. Its workspace is the work, so it is answered nothing and the row is left as
//     it was, with the attempt still there for the pod that replaces it.
//   - A pod not created after the copy is refused (runtime.WorkspaceBundleRefusedOlder), and the
//     refusal spends the attempt: such a pod has been working off its own fetch since before the
//     copy was taken, and a copy left open for the pod after it would be applied over work pushed
//     since. The two instants come from two clocks — podCreated is the creationTimestamp the
//     Kubernetes API server stamped, the row's `at` the node's clock when the shim took the copy —
//     and a creationTimestamp has one-second resolution, so the copy is read to its own second and
//     a pod created inside that second is taken as the newer. A node running ahead of the API
//     server stamps the copy in the replacement pod's future and that pod is refused, which costs
//     the restore and loses no work: the direction to fail in.
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
		if !bundle.Held && !running.Held {
			return nil
		}
		if restore.Push, err = issuePush(ctx, tx, issue); err != nil {
			return err
		}
		bundleAct, runningAct := runtime.DecideWorkspaceRestore(bundle, running, restore.Push)
		for _, decided := range []struct {
			copies copyTable
			act    runtime.WorkspaceCopyAct
		}{{releaseBundleCopies, bundleAct}, {runningCopyCopies, runningAct}} {
			if decided.act == runtime.WorkspaceCopyNotHeld {
				continue
			}
			taken, err := takeCopy(ctx, tx, decided.copies, issue, podUID, podCreated, decided.act)
			if err != nil {
				return err
			}
			if taken.Take != runtime.WorkspaceBundleNone {
				restore.Copies = append(restore.Copies, taken)
			}
		}
		return nil
	})
	if err != nil {
		return runtime.WorkspaceRestore{}, fmt.Errorf("take the saved workspace copies of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	return restore, nil
}

// heldCopy is whether issue holds a copy in copies' table and when the shim took it, the row
// locked for the rest of the restore's transaction.
func heldCopy(ctx context.Context, tx pgx.Tx, copies copyTable, issue string) (runtime.WorkspaceCopyHeld, error) {
	var at time.Time
	err := tx.QueryRow(ctx, fmt.Sprintf(`select at from %s where issue = $1 for update`, copies.table), issue).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceCopyHeld{}, nil
	}
	if err != nil {
		return runtime.WorkspaceCopyHeld{}, fmt.Errorf("read the %s: %w", copies.kind, err)
	}
	return runtime.WorkspaceCopyHeld{Held: true, At: at}, nil
}

// takeCopy spends the one attempt of issue's copy in copies' table on the pod of podUID, as act
// decided, and answers what it recorded: runtime.WorkspaceBundleNone, having recorded nothing,
// when the row already carries its attempt or the pod asking is the pod the work came off. An
// offered copy is sent or refused by the clock rule; a copy the decision refused or superseded
// records that and is sent to nobody. The body is read back only when it is being sent.
func takeCopy(ctx context.Context, tx pgx.Tx, copies copyTable, issue, podUID string, podCreated time.Time, act runtime.WorkspaceCopyAct) (runtime.WorkspaceCopyTaken, error) {
	// The outcome the decision settled, or NULL for the offered copy, whose outcome is the clock
	// rule's below.
	var decided any
	switch act {
	case runtime.WorkspaceCopyRefusedPushed:
		decided = refusedPushedOutcome
	case runtime.WorkspaceCopySuperseded:
		decided = supersededCopyOutcome
	}
	taken := runtime.WorkspaceCopyTaken{Kind: copies.kind}
	b := &taken.Bundle
	var outcome string
	var tips, bookmarks []byte
	err := tx.QueryRow(ctx, fmt.Sprintf(`update %[1]s
		set attempt_pod_uid = $2, attempt_at = now(),
			attempt_outcome = coalesce($4::text,
				case when date_trunc('second', at) <= $3 then '%[3]s' else '%[4]s' end)
		where issue = $1 and attempt_pod_uid is null and source_pod_uid <> $2
		returning attempt_outcome, case when attempt_outcome = '%[3]s' then bundle end, size_bytes,
			at, head, tips, bookmarks, %[2]s, source_pod_uid`, copies.table, copies.id, sentCopyOutcome, refusedOlderCopyOutcome),
		issue, podUID, podCreated, decided).
		Scan(&outcome, &b.Bundle, &taken.Size, &b.At, &b.Head, &tips, &bookmarks, &b.Release, &b.PodUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceCopyTaken{Kind: copies.kind}, nil
	}
	if err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("take the %s: %w", copies.kind, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("read the %s: its tips: %w", copies.kind, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceCopyTaken{}, fmt.Errorf("read the %s: its bookmarks: %w", copies.kind, err)
	}
	switch outcome {
	case sentCopyOutcome:
		taken.Take = runtime.WorkspaceBundleSend
	case refusedOlderCopyOutcome:
		taken.Take = runtime.WorkspaceBundleRefusedOlder
	case refusedPushedOutcome:
		taken.Take = runtime.WorkspaceBundleRefusedPushed
	default:
		taken.Take = runtime.WorkspaceBundleSuperseded
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
	if _, err := s.pool.Exec(ctx, `delete from workspace_running_copies where issue = $1`, issue); err != nil {
		return fmt.Errorf("delete the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// DeleteRestoredRunningWorkspaceCopy drops the running copy of restoreID, which a pod's launcher
// has reported restored. It names the id and not the issue because that is all a report carries
// that is certain: a launcher holds its report until some connection carries it, so one can reach
// the daemon after the issue's cadence has taken a copy of its own, and deleting the issue's row
// on it would take the newer work. Each save gives the copy it stores an id of its own
// (PutRunningWorkspaceCopy), so a report of a copy the issue has replaced deletes nothing and the
// newer copy stays for the issue's next pod. An id below MinRunningWorkspaceCopyRestoreID is a
// release's and matches no row here, which is the disjointness the schema keeps. Deleting a row
// the table no longer holds is success, so a confirmation the daemon read twice finishes.
func (s *Store) DeleteRestoredRunningWorkspaceCopy(ctx context.Context, restoreID int64) error {
	if _, err := s.pool.Exec(ctx, `delete from workspace_running_copies where restore_id = $1`, restoreID); err != nil {
		return fmt.Errorf("delete the restored running workspace copy %d: %w", restoreID, err)
	}
	return nil
}

// DeleteRestoredWorkspaceCopy drops the copy of issue's workspace a pod's launcher reports it
// restored, routing on the restore id alone. That id is all such a report carries that is
// certain of the row: a launcher holds its report until some connection carries it
// (internal/launcher's bundleRestore.owed), so one can reach a daemon that has restarted since
// the send, with no memory of what it sent that pod.
//
// The id says which of the two tables the copy is in, because the two draw from disjoint spaces
// and the schema keeps them so (MinRunningWorkspaceCopyRestoreID): an id at or above the
// boundary is a running copy's, and that delete reads the id alone, since the issue's cadence may
// have taken a newer copy since and that row must stay for the issue's next pod. An id below it
// is the id of the release whose teardown saved the bundle, and that delete names the issue and
// the release together, which is the same rule: a bundle a later release saved stays.
//
// The rule lives here, beside the two deletes it chooses between, because it is a statement about
// which table an id names and about nothing else: the daemon wires this method straight onto the
// runtime's report callback (runtime.WorkspaceBundleRestored), and the sandbox runtime's own
// tests drive the report through it rather than through a copy of the branch.
//
// An id of either kind that no row holds deletes nothing, which is the ordinary end of a late
// report, and the caller is told the delete succeeded, because it did: the row the report
// confirmed is gone.
func (s *Store) DeleteRestoredWorkspaceCopy(ctx context.Context, issue string, restoreID int64) error {
	if restoreID >= MinRunningWorkspaceCopyRestoreID {
		return s.DeleteRestoredRunningWorkspaceCopy(ctx, restoreID)
	}
	return s.DeleteRestoredWorkspaceBundle(ctx, issue, restoreID)
}

// RecordIssuePush records that the branch of issue was pushed to head, as the daemon applied the
// push at at, in the push fact's own transaction tx (workflow's push handler). It is the record
// the restore of a new pod reads to refuse every saved copy of the issue's workspace older than
// that push (TakeWorkspaceCopyForRestore, runtime.DecideWorkspaceRestore), and the migration that
// made the table says which record it is (20261113005308_issue_pushes). One row per issue holds
// the newest push: a push applied after a newer one, a redelivery that slipped past the fact's
// own dedupe or a fact replayed late, moves neither its time nor its head, since the copies a
// guard of that older time would let through are the very copies the newer push makes stale.
func (s *Store) RecordIssuePush(ctx context.Context, tx pgx.Tx, issue, head string, at time.Time) error {
	if issue == "" {
		return errors.New("a push record needs its issue")
	}
	if at.IsZero() {
		return fmt.Errorf("the push record of %s needs when the daemon applied the push", issue)
	}
	if _, err := tx.Exec(ctx, `insert into issue_pushes (issue, pushed_at, head) values ($1, $2, $3)
		on conflict (issue) do update set pushed_at = excluded.pushed_at, head = excluded.head
			where issue_pushes.pushed_at <= excluded.pushed_at`, issue, at, head); err != nil {
		return fmt.Errorf("record the push of %s: %w", issue, err)
	}
	return nil
}

// issuePush is the newest push of issue's branch the daemon recorded (RecordIssuePush), and the
// zero value for an issue with none recorded, which the restore's decision reads as nothing newer
// known to be on GitHub.
func issuePush(ctx context.Context, tx pgx.Tx, issue string) (runtime.WorkspacePush, error) {
	var push runtime.WorkspacePush
	err := tx.QueryRow(ctx, `select pushed_at, head from issue_pushes where issue = $1`, issue).Scan(&push.At, &push.Head)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspacePush{}, nil
	}
	if err != nil {
		return runtime.WorkspacePush{}, fmt.Errorf("read the newest push of %s: %w", issue, err)
	}
	return push, nil
}
