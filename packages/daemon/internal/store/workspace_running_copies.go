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
// (KeepRestoredWorkspaceBundle), an id at or above it names a running copy
// (KeepRestoredRunningWorkspaceCopy). The space is positive because the launcher refuses a
// bundle slice whose release is not (shimwire.LauncherBundle.Validate), so the kind cannot be
// marked by a sign.
const MinRunningWorkspaceCopyRestoreID int64 = 1 << 62

// ErrReleaseBundleSaved is PutRunningWorkspaceCopy's refusal of a copy taken off a pod an idle
// release has saved its bundle off (ReleaseBundleSavedOff). It is a refusal and not a failure:
// nothing went wrong, and the bundle already holds that pod's last copy of the issue's work, so
// the daemon logs it apart from a save that failed.
var ErrReleaseBundleSaved = errors.New("an idle release has saved its bundle off the pod this copy was taken off, and that bundle is the pod's last copy")

// releaseBundleSavedOff is the condition, on issue $1 and pod $2, that the issue's release bundle
// came off that pod. Once an idle release has saved its bundle, that bundle is the pod's last
// copy: no running copy is taken off that pod afterwards, at a push or for any other reason. The
// release's rows run the save first, then a stop per role, then the teardown (daemon's
// idleReleaseRows), so the pod's roles are still live after the save and a push the daemon
// records in that window asks them for a copy; stored, that copy would be the later of the two by
// the clock and would be offered to the next pod in the bundle's place, and the bundle's row,
// closed unsent, would hold the issue's pod from every idle release after it
// (workspaceBundleUnrestored).
//
// It names the pod and not the issue alone because the rule is about that pod. A pod that is not
// the one the bundle came off is never refused by it: a pod that came back without spending the
// bundle's one attempt — its launcher could not read the store, so it started on what was pushed
// — goes on working, and its copies are newer than the bundle and are the restore's to weigh
// (runtime.DecideWorkspaceRestore). Nor does it read the attempt: the pod a bundle came off never
// spends it (TakeWorkspaceCopyForRestore answers that pod nothing), and the pod that does is made
// only once the release has taken that pod away, so for as long as the bundle's row stands the
// bundle is its pod's last copy. A release a wake cancels moves its bundle into the issue's
// running copy (CancelIssueRelease), which lifts the refusal for the pod that then works on, and a
// confirmed restore moves it too (KeepRestoredWorkspaceBundle), keeping its work as the restoring
// pod's running copy.
//
// It is one text, read by the cadence before it asks a shim (ReleaseBundleSavedOff) and written
// into both of PutRunningWorkspaceCopy's statements, so the two places are one rule.
const releaseBundleSavedOff = `exists (select 1 from workspace_bundles where issue = $1 and source_pod_uid = $2)`

// runningCopyReplaced is the part of every statement that writes other work into the row of an
// issue's running copy: the row takes a restore id of its own and its restore attempt is cleared.
// The work is not the copy it replaces, so it is offered to a pod of its own
// (TakeWorkspaceCopyForRestore), and a report naming the id it replaced changes nothing
// (KeepRestoredRunningWorkspaceCopy). It is one text, written by the save of a copy
// (PutRunningWorkspaceCopy), by the move of a release bundle into the table
// (keepReleaseBundleAsRunningCopy, which a confirmed restore and a cancelled release both make)
// and by the keep of a restored running copy (KeepRestoredRunningWorkspaceCopy), so no statement
// gives the row other work under the id it had or leaves an attempt spent on work the row no
// longer holds.
const runningCopyReplaced = `restore_id = nextval('workspace_running_copy_restore_ids'),
	attempt_pod_uid = null, attempt_at = null, attempt_outcome = null`

// runningCopyUpsert is the conflict clause of the two statements that write a whole copy into an
// issue's running copy, the save (PutRunningWorkspaceCopy) and the move of a release bundle
// (keepReleaseBundleAsRunningCopy): one issue has one row, and the copy the insert carries
// replaces every column of the work the row held, under runningCopyReplaced.
const runningCopyUpsert = `on conflict (issue) do update set source_pod_uid = excluded.source_pod_uid,
	bundle = excluded.bundle, size_bytes = excluded.size_bytes, at = excluded.at, head = excluded.head,
	tips = excluded.tips, bookmarks = excluded.bookmarks, ` + runningCopyReplaced

// ReleaseBundleSavedOff is whether issue's release bundle came off pod (releaseBundleSavedOff),
// which the daemon's cadence reads before it asks a shim of that pod for a copy, so that no bundle
// is built off a pod an idle release has saved its bundle off. A pod of "" is no pod, and no row
// names one.
func (s *Store) ReleaseBundleSavedOff(ctx context.Context, issue, pod string) (bool, error) {
	var saved bool
	if err := s.pool.QueryRow(ctx, `select `+releaseBundleSavedOff, issue, pod).Scan(&saved); err != nil {
		return false, fmt.Errorf("read whether the release bundle of %s came off pod %s: %w", issue, pod, err)
	}
	return saved, nil
}

// PutRunningWorkspaceCopy saves the unpushed work of issue's workspace as the copy taken while its
// pod runs, replacing whatever the issue's last copy left: one issue has one running copy, the
// newest, and a copy of an older minute must never be restored in place of a newer one. The copy
// is written to a table of its own and never to the row an idle release saves, because the mere
// presence of that row holds the issue's pod (workspaceBundleUnrestored,
// workflow.IdleHoldBundleUnrestored) and a copy of a live disk's work would hold every working
// issue's pod for ever, as well as destroying a release bundle whose restore no pod has confirmed.
//
// Each copy the save stores takes a restore id of its own and clears the restore attempt the row
// carried with the rest of it (runningCopyReplaced).
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
//
// A copy off a pod the issue's release bundle came off is refused with ErrReleaseBundleSaved
// (releaseBundleSavedOff), and the refusal is a condition of each statement that would write the
// row, so no read between the check and the write can let a copy land after the bundle's save.
// The daemon's single flight keeps the save from running beside a copy's write in the first
// place (runningCopies.hold), so a copy that wrote before the save is dropped by it
// (PutWorkspaceBundle), and one whose write comes after it meets this refusal.
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
	// at the cost of one indexed read that touched nothing. A copy the release bundle refuses
	// moves nothing here either, and the write below refuses it by the same condition.
	moved, err := s.pool.Exec(ctx, `update workspace_running_copies set at = $3
		where issue = $1 and source_pod_uid = $2 and head = $4 and size_bytes = $5
			and tips = $6 and bookmarks = $7 and not `+releaseBundleSavedOff,
		issue, b.PodUID, b.At, b.Head, b.Size(), tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	if moved.RowsAffected() > 0 {
		return nil
	}
	// The insert is a select so that it can carry the refusal's condition: a statement of values
	// has no condition of its own, and the conflict's condition guards the update alone.
	written, err := s.pool.Exec(ctx, `insert into workspace_running_copies
			(issue, source_pod_uid, bundle, size_bytes, at, head, tips, bookmarks)
		select $1, $2, $3, $4, $5, $6, $7, $8
		where not `+releaseBundleSavedOff+`
		`+runningCopyUpsert,
		issue, b.PodUID, b.Bundle, b.Size(), b.At, b.Head, tips, bookmarks)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	if written.RowsAffected() == 0 {
		return fmt.Errorf("save the running workspace copy of %s off pod %s: %w", issue, b.PodUID, ErrReleaseBundleSaved)
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
// says which of the issue's two copies it confirms (KeepRestoredWorkspaceCopy).
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
// work that has since been pushed, and the next pod must never restore it. The two writes that
// invalidate what the issue holds drop the copy inside their own transaction, with the release
// bundle each saves or drops (PutWorkspaceBundle, DeleteWorkspaceBundle). Deleting one the table
// does not hold is success, so a retried delete finishes.
func (s *Store) DeleteRunningWorkspaceCopy(ctx context.Context, issue string) error {
	if err := deleteRunningCopy(ctx, s.pool, issue); err != nil {
		return fmt.Errorf("delete the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// deleteRunningCopy drops issue's running copy whatever minute took it, through db: the pool for
// the cadence's own delete (DeleteRunningWorkspaceCopy), and the transaction of each write that
// drops the copy with what it changes (PutWorkspaceBundle, DeleteWorkspaceBundle). Deleting one
// the table does not hold is success.
func deleteRunningCopy(ctx context.Context, db execer, issue string) error {
	_, err := db.Exec(ctx, `delete from workspace_running_copies where issue = $1`, issue)
	return err
}

// KeepRestoredRunningWorkspaceCopy keeps issue's running copy of restoreID, which the launcher of
// the pod of podUID has reported restored, as that pod's own copy of the work: in one statement
// it names that pod as the row's source, gives the row a restore id of its own and clears its
// attempt (runningCopyReplaced), leaving the work as it is.
//
// The copy is kept rather than dropped because from the confirmation on its work is on the
// restoring pod's disk alone, and none of the three triggers of a running copy need fire for a
// pod that does nothing: a relaunched agent comes up Ready rather than ending a turn, the cadence
// copies only an issue in a turn, and the push copy needs a push. Dropped, the work would go with
// that pod should the cluster end it again before its first turn. Sourced to the restoring pod,
// the copy is answered nothing to that pod, whose workspace already holds it (the take's source
// rule, runtime.DecideWorkspaceRestore), and its new id and clear attempt make it offered to the
// issue's next pod as any copy taken off a running pod is. The pod's own copies replace it as they
// are taken (PutRunningWorkspaceCopy), an idle release of that pod drops it in the save's own
// transaction (PutWorkspaceBundle), and a workspace that bundles to nothing drops it
// (DeleteRunningWorkspaceCopy).
//
// The kept copy is stamped with the statement's now() rather than the time the shim took it,
// for the reason a kept release bundle is (KeepRestoredWorkspaceBundle): the row stands for the
// restoring pod's workspace as it is once the restore is confirmed, and under the old time a push
// recorded after it would refuse the kept copy to the next pod
// (runtime.WorkspaceBundleRefusedPushed) and lose the work again. A push recorded after the
// confirmation takes a copy of its own off the restoring pod's live roles, so the push rule keeps
// its meaning.
//
// The id says which copy, because a launcher holds its report until some connection carries it,
// so one can reach the daemon after the issue's cadence has taken a copy of its own, and a row
// matched on the issue alone would be the newer work. Each save gives the copy it stores an id of
// its own (PutRunningWorkspaceCopy), and so does this keep, so a report of a copy the issue has
// replaced, and a confirmation the daemon read twice, change nothing. The issue bounds the id to
// the work of the launcher that reports it: the session the report came over is a pod of issue
// (the Secret its launcher authenticated with names the issue), and an id of another issue's copy
// matches no row, so a launcher of one issue cannot rebind the work of another. An id below
// MinRunningWorkspaceCopyRestoreID is a release's and matches no row here, which is the
// disjointness the schema keeps. A report with no pod to name is refused, since the row could not
// then tell the pod the work is on from the pod that replaces it.
//
// It keeps only a copy whose one attempt was a send to that pod: the row's attempt names podUID
// and records the send (attempt_outcome, TakeWorkspaceCopyForRestore). Any other launcher of the
// issue — one whose pod the take answered nothing, refused the copy or closed it superseded —
// reports work its pod was never sent, and its report changes nothing, so the row is never sourced
// to a pod whose disk does not hold the work, and the attempt stays as the take left it.
func (s *Store) KeepRestoredRunningWorkspaceCopy(ctx context.Context, issue, podUID string, restoreID int64) error {
	if podUID == "" {
		return fmt.Errorf("the restored running workspace copy %d of %s needs the pod that restored it", restoreID, issue)
	}
	if _, err := s.pool.Exec(ctx, `update workspace_running_copies
		set source_pod_uid = $3, at = now(), `+runningCopyReplaced+`
		where issue = $1 and restore_id = $2 and attempt_pod_uid = $3 and attempt_outcome = $4`,
		issue, restoreID, podUID, sentCopyOutcome); err != nil {
		return fmt.Errorf("keep the restored running workspace copy %d of %s as the copy of pod %s: %w", restoreID, issue, podUID, err)
	}
	return nil
}

// KeepRestoredWorkspaceCopy keeps the copy of issue's workspace the launcher of the pod of podUID
// reports it restored as that pod's own running copy, routing on the restore id. That id is all
// such a report carries that is certain of the row: a launcher holds its report until some
// connection carries it (internal/launcher's bundleRestore.owed), so one can reach a daemon that
// has restarted since the send, with no memory of what it sent that pod.
//
// The id says which of the two tables the copy is in, because the two draw from disjoint spaces
// and the schema keeps them so (MinRunningWorkspaceCopyRestoreID). An id at or above the boundary
// is a running copy's, and that keep names the issue and the id together: the id because the
// issue's cadence may have taken a newer copy since and that row must stay as it is, and the issue
// because the report is that issue's launcher's and another issue's copy is none of its business.
// An id below it is the id of the release whose teardown saved the bundle, and that keep names the
// issue and the release together, which is the same rule: a bundle a later release saved stays.
// Each keep also reads the copy's attempt, and keeps nothing that was not sent to the reporting
// pod.
//
// The rule lives here, beside the two keeps it chooses between, because it is a statement about
// which table an id names and about nothing else: the daemon wires this method straight onto the
// runtime's report callback (runtime.WorkspaceBundleRestored), and the sandbox runtime's own tests
// drive the report through it rather than through a copy of the branch.
//
// An id of either kind that no row of issue holds, or that names a copy not sent to podUID,
// changes nothing, which is the ordinary end of a late report, and the caller is told the keep
// succeeded, because it did: the work the report confirmed is either kept already, replaced by
// newer work, or never this pod's.
func (s *Store) KeepRestoredWorkspaceCopy(ctx context.Context, issue, podUID string, restoreID int64) error {
	if restoreID >= MinRunningWorkspaceCopyRestoreID {
		return s.KeepRestoredRunningWorkspaceCopy(ctx, issue, podUID, restoreID)
	}
	return s.KeepRestoredWorkspaceBundle(ctx, issue, podUID, restoreID)
}
