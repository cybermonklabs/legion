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
// pod of its own (TakeRunningWorkspaceCopyForRestore), and a report naming the id it replaced
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

// TakeRunningWorkspaceCopyForRestore answers the running copy of issue for the pod of podUID,
// created at podCreated, whose launcher it is about to be sent to, and records in the same
// statement what that ask did to the copy. The bundle it answers carries the row's restore id as
// its Release (runtime.WorkspaceBundle.Release), which the restore sends to the launcher and reads
// back out of its report: the id is above every id a release holds
// (MinRunningWorkspaceCopyRestoreID), so a report that reaches the daemon after a restart still
// says which of the issue's two copies it confirms.
//
// It answers runtime.WorkspaceBundleNone, having recorded nothing, when the issue has no running
// copy — one never taken, one whose restore a launcher confirmed, one an invalidating path dropped
// — when the row already carries its attempt, and when the pod asking is the pod the work was
// taken off (source_pod_uid). That last is the ordinary case rather than a corner: a copy is taken
// while the pod runs, so the pod it came off is still there and still starting roles, and every
// start of every role asks for a copy to restore (sandbox's launchers.start). Its workspace is the
// work, it needs nothing put back, and a copy applied to it would `jj edit` back to the copy's head
// and move each bookmark `--allow-backwards` over the work done since the copy was taken. The row
// is left as it was, so the attempt is still there for the pod that replaces it.
//
// One attempt per copy is the rule, and this statement is where it is kept, for the reason the
// release bundle keeps it (TakeWorkspaceBundleForRestore): the launcher's own record of the
// restore it ran dies with the process that ran it, so a roles container that restarts on the
// pod's own disk comes back with the record gone and the workspace hours of work past the copy.
// The attempt is written before any slice leaves the daemon, so a send that fails halfway, a
// restore that fails and a restore whose report never arrived all leave the same row: one no pod is
// sent again.
//
// What the first pod that is not the source is answered is podCreated's rule. A pod created after
// the copy was taken is a pod that replaced the one it came off, and it is sent the work
// (runtime.WorkspaceBundleSend). A pod that is not is refused (runtime.WorkspaceBundleRefusedOlder)
// and the refusal spends the attempt: such a pod has been working off its own fetch since before
// the copy was taken, and a copy left open for the pod after it would be applied over work pushed
// since. The two instants come from two clocks — podCreated is the creationTimestamp the
// Kubernetes API server stamped, the row's `at` is the node's clock when the shim took the copy —
// so the copy is read to its own second, a pod created inside that second is taken as the newer,
// and a node running ahead of the API server costs a restore rather than moving an issue's
// bookmarks backwards.
//
// Unlike a release bundle, a running copy nothing restored holds no pod. The work it holds is on
// the disk it was taken off, so an idle release of that pod saves it again as a release bundle,
// and the idle rule reads workspace_bundles alone (workspaceBundleUnrestored).
func (s *Store) TakeRunningWorkspaceCopyForRestore(ctx context.Context, issue, podUID string, podCreated time.Time) (runtime.WorkspaceBundle, runtime.WorkspaceBundleTake, error) {
	if podUID == "" {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("the restore attempt of the running workspace copy of %s needs the pod it is sent to", issue)
	}
	if podCreated.IsZero() {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("the restore attempt of the running workspace copy of %s needs when the pod %s it is sent to was created", issue, podUID)
	}
	var b runtime.WorkspaceBundle
	var outcome string
	var tips, bookmarks []byte
	// The body is read back only when it is being sent: a refusal records the attempt and
	// answers the row's facts alone, which is what the warning it writes names.
	err := s.pool.QueryRow(ctx, `update workspace_running_copies
		set attempt_pod_uid = $2, attempt_at = now(),
			attempt_outcome = case when date_trunc('second', at) <= $3 then 'sent' else 'refused_older' end
		where issue = $1 and attempt_pod_uid is null and source_pod_uid <> $2
		returning attempt_outcome, case when attempt_outcome = 'sent' then bundle end,
			at, head, tips, bookmarks, restore_id, source_pod_uid`,
		issue, podUID, podCreated).Scan(&outcome, &b.Bundle, &b.At, &b.Head, &tips, &bookmarks, &b.Release, &b.PodUID)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, nil
	}
	if err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("take the running workspace copy of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	if err := json.Unmarshal(tips, &b.Tips); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the running workspace copy of %s: its tips: %w", issue, err)
	}
	if err := json.Unmarshal(bookmarks, &b.Bookmarks); err != nil {
		return runtime.WorkspaceBundle{}, runtime.WorkspaceBundleNone, fmt.Errorf("read the running workspace copy of %s: its bookmarks: %w", issue, err)
	}
	// The outcome the statement wrote is the answer: `sent` for the body above, `refused_older`
	// for the pod it refused, whose attempt is spent all the same.
	if outcome != "sent" {
		return b, runtime.WorkspaceBundleRefusedOlder, nil
	}
	return b, runtime.WorkspaceBundleSend, nil
}

// RunningWorkspaceCopy is the facts of issue's running copy without its bytes, Held false when
// the issue holds none. The restore of a new pod compares those facts with the issue's release
// bundle to decide which of the two to take, and the cadence reads the size it stored to log it;
// a copy runs to tens of megabytes, so reading at and size_bytes rather than the row keeps those
// bytes out of the pool and out of the daemon's memory.
//
// It answers the runtime's own shape (runtime.WorkspaceCopyFacts), as the release bundle's
// reader does (WorkspaceBundleFacts): the two are compared against each other and handed to the
// runtime as one pair (SavedWorkspaceCopies), which has no business knowing which table either
// came off, and a struct of this package holding the same two facts would exist only to be
// copied into that one.
func (s *Store) RunningWorkspaceCopy(ctx context.Context, issue string) (runtime.WorkspaceCopyFacts, error) {
	var facts runtime.WorkspaceCopyFacts
	err := s.pool.QueryRow(ctx, `select at, size_bytes from workspace_running_copies where issue = $1`, issue).
		Scan(&facts.At, &facts.Size)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceCopyFacts{}, nil
	}
	if err != nil {
		return runtime.WorkspaceCopyFacts{}, fmt.Errorf("read the running workspace copy of %s: %w", issue, err)
	}
	facts.Held = true
	return facts, nil
}

// DeleteRunningWorkspaceCopy drops issue's running copy whatever minute took it. It is the delete
// of every path that invalidates what the issue holds — a release that found nothing unpushed, a
// cancelled release, an issue or tree whose work is over — each of which exists to stop the
// issue's next pod restoring work that is no longer the issue's, and a copy taken minutes before
// would be the newest such work. Deleting one the table does not hold is success, so a retried
// delete finishes.
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

// SavedWorkspaceCopies is what both copies of issue's workspace say of themselves, neither
// carrying its bytes: the bundle its last idle release saved (WorkspaceBundleFacts) and the copy
// its cadence took while the pod ran (RunningWorkspaceCopy). It is the read the restore of a new
// pod decides on (runtime.SavedWorkspaceCopies, sandbox's launchers.restoreWorkspace), and it is
// one call because that decision is one question: which of the two holds the newer work.
//
// It reads rather than takes because a take is spent. Each copy is offered to one pod only, and
// the ask that reaches that pod closes it whether a byte was sent or not
// (TakeWorkspaceBundleForRestore, TakeRunningWorkspaceCopyForRestore), so a restore that decided
// by taking would spend the loser's one attempt on a copy it never sent, and the work of that
// copy would be closed to the pod that could still have used it.
//
// The two reads are two statements and not one join: they are two tables keyed by the issue, the
// rows are independent of each other, and an issue that holds one holds it whatever the other
// table says. The restore runs once per pod, before its first role, so two indexed reads there
// cost nothing worth a join's shape.
func (s *Store) SavedWorkspaceCopies(ctx context.Context, issue string) (bundle, running runtime.WorkspaceCopyFacts, err error) {
	if bundle, err = s.WorkspaceBundleFacts(ctx, issue); err != nil {
		return runtime.WorkspaceCopyFacts{}, runtime.WorkspaceCopyFacts{}, err
	}
	if running, err = s.RunningWorkspaceCopy(ctx, issue); err != nil {
		return runtime.WorkspaceCopyFacts{}, runtime.WorkspaceCopyFacts{}, err
	}
	return bundle, running, nil
}
