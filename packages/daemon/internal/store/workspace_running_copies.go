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

// ErrNewerCopyStands is PutRunningWorkspaceCopy's refusal of a copy whose take began before the
// take of the running copy the issue holds. It is a refusal and not a failure: the row stands for
// a workspace read later than this copy read its own, so the daemon logs it apart from a save
// that failed.
var ErrNewerCopyStands = errors.New("the issue's running copy was taken after this copy's take began, and a copy never replaces a newer one")

// ErrIssueNotRecorded is the refusal of either write of an issue's running copy, the save
// (PutRunningWorkspaceCopy) and the cadence's delete (DeleteRunningWorkspaceCopy), by a project
// that does not record the issue (projectRecordsIssue). It is a refusal and not a failure: the
// row is another project's to write or no project's, so the daemon logs it apart from a write
// that failed.
var ErrIssueNotRecorded = errors.New("the project does not record the issue, and only a project that records an issue writes its running copy")

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
// the one the bundle came off is never refused by it: a pod that came back without being sent the
// bundle — its launcher could not read the store, so it started on what was pushed
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
// replaces every column of the work the row held, both its times among them, under
// runningCopyReplaced. The save follows it with a condition of its own, that the row's take did
// not begin after the copy's; the move weighs the two copies before it writes.
const runningCopyUpsert = `on conflict (issue) do update set source_pod_uid = excluded.source_pod_uid,
	bundle = excluded.bundle, size_bytes = excluded.size_bytes, at = excluded.at,
	written_at = excluded.written_at, head = excluded.head,
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

// WorkspaceCopyBegun is when the take of a copy of an issue's workspace began, by the store's
// clock (BeginWorkspaceCopy). Each write of a copy stores it as the copy's `at`
// (PutRunningWorkspaceCopy, PutWorkspaceBundle), which the restore orders the issue's two copies
// by and refuses a running copy older than the newest recorded push by
// (runtime.DecideWorkspaceRestore).
//
// Its time is unexported so that the store alone makes one that holds a time: no caller can hand
// a write the shim's clock, the daemon's or a time of its own choosing. The zero value, which any
// caller can make, holds none, and each write refuses it.
type WorkspaceCopyBegun struct {
	at time.Time
}

// At is the store's time the take began at, which a log line names as the copy's time.
func (b WorkspaceCopyBegun) At() time.Time { return b.at }

// BeginWorkspaceCopy reads the store's clock as the take of a copy of an issue's workspace begins.
// The taker calls it before it asks a shim's bundler to read the workspace, and once it holds the
// issue's one place in flight: the cadence's copy (internal/daemon's takeRunningCopy) and an idle
// release's save (internal/daemon's bundleIssueWorkspace). The time it answers is the copy's `at`.
//
// A copy's time is when its take began and not when the store wrote it, because the bundler reads
// the workspace after the take begins and the write lands later, once the bundle was built and
// carried. A push the daemon records between the two is then after the copy's time, and the
// restore refuses the copy (runtime.DecideWorkspaceRestore), which can have read the workspace
// before the pushed commits were made. Stamped at its write, such a copy would carry a time after
// the push's record and pass that rule. The daemon also stores no copy a push overtook while it
// was being built (internal/daemon's runningCopies.pushed), but it marks the copy only once the
// push's transaction has committed, so a copy whose check runs just before that mark and whose
// write lands just after the push's record is stored all the same; its time is what refuses it.
//
// It is clock_timestamp(), this database's clock as the statement runs; the push's record
// (RecordIssuePush), a pod's first ask (TakeWorkspaceCopyForRestore) and every write of a copy read
// that same clock, so the restore's choice between the two copies compares one clock alone (the
// drop for a pod of a kept Sandbox alone reads the API server's too, runtime.DecideKeptVolumeDrop).
// A read that fails answers no time, and the taker takes no copy.
func (s *Store) BeginWorkspaceCopy(ctx context.Context) (WorkspaceCopyBegun, error) {
	var at time.Time
	if err := s.pool.QueryRow(ctx, `select clock_timestamp()`).Scan(&at); err != nil {
		return WorkspaceCopyBegun{}, fmt.Errorf("read the store's clock as the take of a workspace copy begins: %w", err)
	}
	return WorkspaceCopyBegun{at: at}, nil
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
// The copy carries two times, both the store's clock, and keeps none of the time the shim read the
// workspace (runtime.WorkspaceBundle.At), by the clock of the node its pod runs on. Its `at` is
// begun, when its take began (BeginWorkspaceCopy), which the restore orders the issue's two copies
// by and compares with the newest push's record. Its written_at is when the save wrote it, the
// store's clock read once the save holds the issue's copy lock (lockWorkspaceCopies), which the
// restore compares with the asking pod's first ask, recorded under the same lock: a copy written
// after that ask is left for the next pod even when its take began before it
// (runtime.DecideWorkspaceRestore). Every time the restore compares is then this one clock, and no
// skew between a node, the API server and the database can turn the choice. A begun that holds no
// time is refused, so no copy is written without the time its take began. The save's two
// statements are one transaction under that lock, so the write time either writes is the time the
// copy became the issue's.
//
// A save whose content equals the stored row's moves the two times alone, `at` to its take's begin
// and written_at to the store's clock under the lock, and leaves the bytes, the size and the
// restore id where they are. Equal means the same head, the same tips, the same bookmarks and the
// same size, taken off the same pod. A turn that reads the workspace and writes nothing in it ends
// with the work the copy before it already holds, and rewriting a multi-megabyte row every five
// minutes to store the bytes again is waste: the row that is already there is the same copy, so
// the save only records that the copy is current as of a later minute. Its attempt stays as it
// was with the rest of it — nothing about the work has changed, so the pod that was already
// offered it is still the pod this copy was offered to — and the first copy whose content
// differs is a new row with a new id and an attempt of its own. The pod is part
// of the comparison because the rest of the row is read against it: a workspace that comes back
// unchanged on a pod that replaced the first would otherwise keep naming the pod that is gone as
// its source, and the take would then answer the copy to the pod it was taken off.
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
//
// A copy never replaces a stored running copy whose take began later: one whose `at` is before
// the row's is refused with ErrNewerCopyStands and leaves the row as it is. The daemon's single
// flight writes the cadence's own copies in the order their takes began, so the row such a copy
// meets is one written at a move — a cancelled release's (CancelIssueRelease) or a confirmed
// restore's (KeepRestoredWorkspaceBundle, KeepRestoredRunningWorkspaceCopy), each stamped with
// the move's own time — or a copy off another pod. Stored, the older copy would put back the work
// of its own earlier read over the work the row holds, and would carry a time from before a push
// the row's time is after, which the push rule then refuses (runtime.DecideWorkspaceRestore).
// Both statements carry the condition under the issue's copy lock, and where the release
// bundle's refusal holds as well, that refusal is the one answered.
//
// The rule closes two kinds of order, each of which the store alone would otherwise let through
// whatever its caller does (TestEveryInterleavingOfTheCopysTimesKeepsBothInvariants). In one, a
// release's save falls between a copy's take and its write, and the bundle leaves its table
// before the write: in `take push ask confirm save cancel write` the copy taken before the save
// would replace the copy the cancel moved, dropping the work done between the take and the save.
// In the other, a copy is taken after a release's save and written after the cancel moved the
// bundle: in `save take push ask cancel write confirm` the copy would replace the moved copy
// with a time from before a push recorded during the release, and the push rule would refuse it
// to the next pod. The rule also keeps a confirmed restore's copy over a late copy, one read off
// the pod before the restoring pod and written after the confirmation; that late copy's work is
// lost to the restoring pod's own copy, which is the late-copy loss the restore accepts
// (runtime.DecideWorkspaceRestore).
//
// project is the token of the daemon whose cadence took the copy (claim.ProjectToken), and the
// save writes only where project records issue (projectRecordsIssue), the rule the take and the
// drop of the issue's copies keep (TakeWorkspaceCopyForRestore, DropHeldWorkspaceCopies). Where
// it does not, which the save reads under the issue's copy lock before either statement, it
// writes nothing and answers ErrIssueNotRecorded. The daemon's cadence copies a pod of every
// claim of its own project (internal/daemon's runningCopyClaim), and an operator's spawn makes
// such a claim of an issue another project records. One issue has one running copy and the row
// names no project, so on a database the two projects' daemons share, the spawn's copy would
// replace the running copy of the project that records the issue, and that project's next pod
// would be sent the spawn's work in place of its own. The rule is the store's, so it holds
// whoever calls the save, and an empty project is refused, as the take refuses one.
func (s *Store) PutRunningWorkspaceCopy(ctx context.Context, project, issue string, begun WorkspaceCopyBegun, b runtime.WorkspaceBundle) error {
	if issue == "" {
		return errors.New("a running workspace copy needs its issue")
	}
	if project == "" {
		return fmt.Errorf("the running workspace copy of %s needs the project whose cadence took it", issue)
	}
	if b.PodUID == "" {
		return fmt.Errorf("the running workspace copy of %s needs the uid of the pod its work came off", issue)
	}
	if len(b.Bundle) == 0 {
		return fmt.Errorf("the running workspace copy of %s carries no work", issue)
	}
	if begun.at.IsZero() {
		return fmt.Errorf("the running workspace copy of %s needs the store's time its take began (BeginWorkspaceCopy)", issue)
	}
	tips, bookmarks, err := bundleJSON(b)
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	err = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkspaceCopies(ctx, tx, issue); err != nil {
			return err
		}
		if recorded, err := projectRecordsIssue(ctx, tx, project, issue); err != nil {
			return err
		} else if !recorded {
			return fmt.Errorf("project %s: %w", project, ErrIssueNotRecorded)
		}
		// The unchanged workspace is answered first and without its bytes, so the common copy
		// of a turn that changed nothing neither carries tens of megabytes to the database nor
		// writes them again. A workspace that did change matches no row here and is written
		// whole below, at the cost of one indexed read that touched nothing. A copy the release
		// bundle refuses moves nothing here either, and the write below refuses it by the same
		// condition, as it does a copy whose take began before the row's.
		moved, err := tx.Exec(ctx, `update workspace_running_copies set at = $7, written_at = clock_timestamp()
			where issue = $1 and source_pod_uid = $2 and head = $3 and size_bytes = $4
				and tips = $5 and bookmarks = $6 and at <= $7 and not `+releaseBundleSavedOff,
			issue, b.PodUID, b.Head, b.Size(), tips, bookmarks, begun.at)
		if err != nil || moved.RowsAffected() > 0 {
			return err
		}
		// The insert is a select so that it can carry the refusal's condition: a statement of
		// values has no condition of its own, and the conflict's condition guards the update
		// alone, which is where the newer row is kept.
		written, err := tx.Exec(ctx, `insert into workspace_running_copies
				(issue, source_pod_uid, bundle, size_bytes, at, written_at, head, tips, bookmarks)
			select $1, $2, $3, $4, $8, clock_timestamp(), $5, $6, $7
			where not `+releaseBundleSavedOff+`
			`+runningCopyUpsert+`
			where workspace_running_copies.at <= excluded.at`,
			issue, b.PodUID, b.Bundle, b.Size(), b.Head, tips, bookmarks, begun.at)
		if err != nil {
			return err
		}
		if written.RowsAffected() > 0 {
			return nil
		}
		// Nothing was written, so the release bundle's condition or the newer row's held, and
		// the bundle's is read again to say which, so that it is the one answered where both
		// held. The copy is refused either way.
		var saved bool
		if err := tx.QueryRow(ctx, `select `+releaseBundleSavedOff, issue, b.PodUID).Scan(&saved); err != nil {
			return err
		}
		if saved {
			return fmt.Errorf("off pod %s: %w", b.PodUID, ErrReleaseBundleSaved)
		}
		return fmt.Errorf("taken at %s: %w", begun.at, ErrNewerCopyStands)
	})
	if err != nil {
		return fmt.Errorf("save the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// The two tables a saved copy of an issue's workspace is kept in, and the column each keeps the
// copy's restore id in, with the attempt_outcome each take that spends an attempt records. The
// restore's statements are written once for both tables and formatted with these constants and
// nothing else, so the read the decision is made from and the write that records it are one text
// whichever table a copy is in. Nothing writes `refused_older`, the outcome an earlier daemon
// recorded on a copy it refused to a pod created before the copy was taken: a copy written after
// the asking pod's first ask is now left for the next pod with nothing recorded
// (runtime.WorkspaceBundleLeftForNextPod), and the two tables' checks admit it for the rows an
// earlier daemon wrote.
const (
	workspaceBundlesTable  = "workspace_bundles"
	workspaceBundlesID     = "release_id"
	runningCopiesTable     = "workspace_running_copies"
	runningCopiesRestoreID = "restore_id"
	refusedPushedOutcome   = "refused_pushed"
	supersededCopyOutcome  = "superseded"
	sentCopyOutcome        = "sent"
)

// copyOutcomes is the attempt_outcome each take that spends an attempt records, and the one place
// a take and its outcome meet. A take it does not name fails the take (takeCopy) rather than
// record an outcome this file was not written for.
var copyOutcomes = map[runtime.WorkspaceBundleTake]string{
	runtime.WorkspaceBundleSend:          sentCopyOutcome,
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
// whose launcher it is about to be sent to, and records what that ask did to each copy, all in one
// transaction: it records the pod's first ask for a restore if this is it (firstRestoreAsk), reads
// the issue's release bundle, its running copy and the newest push of its branch the daemon
// recorded, decides between them (runtime.DecideWorkspaceRestore, where every rule of the choice
// is written: the push refuses a running copy whose take began before it and never a release
// bundle, and a copy written after the pod's first ask is left for the next pod), and records the
// take of every copy the decision answered a send, a refusal or a supersession against this pod
// (takeCopy), answering each copy's take in that copy's own field (runtime.WorkspaceRestore). A
// refused or superseded copy is spent from then on, and a sent one once a role of this pod
// registers (heldCopy). It is one transaction because the choice is one decision: made as separate
// statements it could close the older copy and then fail to take the newer, leaving the store
// half-changed and the pod given neither. A failure anywhere in it leaves every row as it was, the
// first ask's included.
//
// project is the token of the daemon whose pod asks (claim.ProjectToken), and an issue's copies
// are project's only where project records the issue (issues.project, the Dispatch key the token
// lowercases). Where it does not, the take answers an empty runtime.WorkspaceRestore and records
// nothing at all: no take of either copy, and not the pod's first ask. Neither table names a
// project, and the mark of the release that saved a bundle, which does, is gone once the release
// has finished, so the issue's record stands in for it. That holds because the one writer of a
// release bundle is the save of an idle release (PutWorkspaceBundle, run by internal/daemon's
// bundleIssueWorkspace from the release's save row): a daemon's outbox runs only the rows of its
// own project's keys (record's DueRows), and a release only marks a pod whose issue its project
// records (ReleaseIdleIssue, readIdleView). So a bundle of an issue this project does not record
// was saved by another project's daemon on a database the two share, for that daemon's own pod,
// and so was the running copy a cancel of that release moves it into (CancelIssueRelease). The
// operator's spawn makes a claim of such an issue under this project's token, and a new pod of
// that claim would otherwise spend the other daemon's attempt — a refusal or a supersession at
// once, a send once a role of the pod registers — and leave that daemon's pod nothing to restore.
// Its first ask could spend it too: each first ask drops all but the issue's newest
// maxWorkspaceRestoreAsks records, and a copy sent to a pod whose record is gone reads as spent
// (heldCopy), so asks of this project's pods could push out the record of the other daemon's pod
// the copy was sent to before any role of it registered.
//
// The cadence copies a pod of every claim of the daemon's own project (internal/daemon's
// runningCopyClaim), the operator's spawn's among them, and both writes of a running copy keep
// this same rule (PutRunningWorkspaceCopy, DeleteRunningWorkspaceCopy), so this daemon's cadence
// stores no copy of an issue this project does not record and drops none, and the next pod of the
// spawn's claim starts on what its fetch provisioned. A row of such an issue that a daemon wrote
// before its writes kept the rule names no project either, and is withheld as another daemon's is.
//
// The take holds the issue's copy lock from its first statement (lockWorkspaceCopies, in the ask
// the take and the drop share, askWorkspaceCopies), so every write of a copy either committed
// before the take read anything or stamps its write time after the take committed. The pod's
// first ask, of an issue project records, is recorded whatever the issue holds, and before
// anything of the copies is read: a pod that asks while the issue holds no copy has begun its work
// all the same, and a copy written after that ask must find it. Every pod's launcher asks at its
// first role start (internal/runtime/sandbox's launchers.start), so a pod that existed before a
// copy was written asked before it. A pod of a Sandbox that owns the issue's kept volume asks the
// drop of the copies that volume holds first, at the same start, and the drop records that first
// ask as the take does (DropHeldWorkspaceCopies). The ask's time is the store's clock read under
// that lock, as a copy's write time is (PutRunningWorkspaceCopy, PutWorkspaceBundle); a copy's
// `at` is the store's clock when its take began (BeginWorkspaceCopy), and a push's time is the
// store's clock when its record was written (RecordIssuePush), so the decision compares one clock
// alone.
//
// Two comparisons of a copy's time with another clock are left, and neither is this decision's.
// One is the drop's, for a pod of a Sandbox that owns the issue's kept volume: it compares a
// copy's `at` with the Sandbox's creationTimestamp, the API server's clock
// (runtime.DecideKeptVolumeDrop, DropHeldWorkspaceCopies), and a skew of seconds does not change
// which copies it drops. The other is on the pod's side: the launcher applies no copy to a
// workspace whose creation record, stamped by its own node's clock, is not after the copy's time
// (internal/launcher's restoreWorkspace, workspace.Created). It must read the node's clock, since
// the workspace is made on the node, and nothing of it reaches the store. Its gap is not seconds:
// the workspace it refuses is the one the copy came out of, made when that pod started, a pod's
// whole life before the copy, and the one it accepts is a later pod's, made at that pod's start,
// after the pod the copy came off had ended; a node clock seconds off the database's moves neither
// across.
//
// Both rows are read `for update`, so a save of either copy that lands while the decision is being
// made waits for it, and the copy the decision read is the copy whose take is recorded. A copy is
// read by the facts the decision reads until it is taken; a copy runs to tens of megabytes, and
// only the one being sent is read with its body. A copy left for the next pod is not written at
// all, and is answered with the time it was written alone, which the restore's line names beside
// the pod's first ask.
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
func (s *Store) TakeWorkspaceCopyForRestore(ctx context.Context, project, issue, podUID string) (runtime.WorkspaceRestore, error) {
	if project == "" {
		return runtime.WorkspaceRestore{}, fmt.Errorf("the restore attempt of the saved workspace copies of %s needs the project whose pod asks", issue)
	}
	if podUID == "" {
		return runtime.WorkspaceRestore{}, fmt.Errorf("the restore attempt of the saved workspace copies of %s needs the pod it is sent to", issue)
	}
	restore, err := s.askWorkspaceCopies(ctx, project, issue, podUID, func(tx pgx.Tx, ask *runtime.WorkspaceRestore, bundle, running runtime.WorkspaceCopyHeld) (bundleTake, runningTake runtime.WorkspaceBundleTake, err error) {
		// Nothing was released and no copy was taken, or every copy has been restored and
		// dropped: the pod starts on what its fetch provisioned, having spent nothing.
		if !bundle.Held() && !running.Held() {
			return runtime.WorkspaceBundleNone, runtime.WorkspaceBundleNone, nil
		}
		if ask.Push, err = issuePush(ctx, tx, issue); err != nil {
			return runtime.WorkspaceBundleNone, runtime.WorkspaceBundleNone, err
		}
		bundleTake, runningTake, ask.Offered = runtime.DecideWorkspaceRestore(bundle, running, ask.Push, podUID, ask.FirstAsked)
		return bundleTake, runningTake, nil
	})
	if err != nil {
		return runtime.WorkspaceRestore{}, fmt.Errorf("take the saved workspace copies of %s for the pod %s to restore: %w", issue, podUID, err)
	}
	return restore, nil
}

// askWorkspaceCopies is one ask of the pod of podUID about issue's two saved copies: what the
// take (TakeWorkspaceCopyForRestore) and the drop of the copies a kept volume holds
// (DropHeldWorkspaceCopies) share, so that a change to the rules of an ask is made here once and
// each of them writes only its decision. It is one transaction, and its first statement takes the
// issue's copy lock (lockWorkspaceCopies). Where project does not record issue
// (projectRecordsIssue), it answers an empty runtime.WorkspaceRestore and records nothing, the
// pod's first ask included. Otherwise it records the pod's first ask if this is it
// (firstRestoreAsk), reads both copies for update (heldCopy), and has decide answer what the ask
// does to each. decide fills the rest of the answer itself — the kind of the copy offered, or of
// the newer copy — and may read more of tx, as the take reads the issue's newest push. The walk
// then records each copy's take against the pod: nothing for WorkspaceBundleNone, the copy's two
// times alone for WorkspaceBundleLeftForNextPod, the delete of a copy the pod's kept volume holds
// (dropCopy), and the take of every other (takeCopy). A failure anywhere leaves every row as it
// was, the first ask's included, and is answered unwrapped, so the caller names the act that
// failed.
func (s *Store) askWorkspaceCopies(ctx context.Context, project, issue, podUID string,
	decide func(tx pgx.Tx, ask *runtime.WorkspaceRestore, bundle, running runtime.WorkspaceCopyHeld) (bundleTake, runningTake runtime.WorkspaceBundleTake, err error),
) (runtime.WorkspaceRestore, error) {
	var ask runtime.WorkspaceRestore
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		ask = runtime.WorkspaceRestore{}
		if err := lockWorkspaceCopies(ctx, tx, issue); err != nil {
			return err
		}
		// The issue's copies are no copies of this project's: nothing is answered and nothing
		// recorded, the pod's first ask included.
		if recorded, err := projectRecordsIssue(ctx, tx, project, issue); err != nil || !recorded {
			return err
		}
		var err error
		if ask.FirstAsked, err = firstRestoreAsk(ctx, tx, issue, podUID); err != nil {
			return err
		}
		bundle, err := heldCopy(ctx, tx, releaseBundleCopies, issue)
		if err != nil {
			return err
		}
		running, err := heldCopy(ctx, tx, runningCopyCopies, issue)
		if err != nil {
			return err
		}
		bundleTake, runningTake, err := decide(tx, &ask, bundle, running)
		if err != nil {
			return err
		}
		for _, decided := range []struct {
			copies copyTable
			held   runtime.WorkspaceCopyHeld
			take   runtime.WorkspaceBundleTake
			taken  *runtime.WorkspaceCopyTaken
		}{{releaseBundleCopies, bundle, bundleTake, &ask.ReleaseBundle}, {runningCopyCopies, running, runningTake, &ask.Running}} {
			switch decided.take {
			case runtime.WorkspaceBundleNone:
				continue
			case runtime.WorkspaceBundleLeftForNextPod:
				// Nothing is written: the copy's attempt is the next pod's. Its two times are
				// answered, the write being the one the decision weighed against the pod's first ask.
				*decided.taken = runtime.WorkspaceCopyTaken{Take: decided.take,
					Bundle: runtime.WorkspaceBundle{At: decided.held.At}, Written: decided.held.Written}
				continue
			case runtime.WorkspaceBundleHeldByVolume:
				*decided.taken, err = dropCopy(ctx, tx, decided.copies, issue)
			default:
				*decided.taken, err = takeCopy(ctx, tx, decided.copies, issue, podUID, decided.take)
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return runtime.WorkspaceRestore{}, err
	}
	return ask, nil
}

// projectRecordsIssue is whether project, a claim.ProjectToken, records issue (issues.project,
// the Dispatch key the token lowercases), which is what makes an issue's saved copies project's to
// take (TakeWorkspaceCopyForRestore) and to drop (DropHeldWorkspaceCopies), and its running copy
// project's to write (PutRunningWorkspaceCopy) and to delete (DeleteRunningWorkspaceCopy).
func projectRecordsIssue(ctx context.Context, tx pgx.Tx, project, issue string) (bool, error) {
	var recorded bool
	if err := tx.QueryRow(ctx, `select exists (select 1 from issues where key = $1 and lower(project) = $2)`,
		issue, project).Scan(&recorded); err != nil {
		return false, fmt.Errorf("read whether project %s records %s: %w", project, issue, err)
	}
	return recorded, nil
}

// workspaceCopyLock is the statement that takes issue $1's copy lock until its transaction ends
// (lockWorkspaceCopies).
const workspaceCopyLock = `select pg_advisory_xact_lock(hashtextextended('workspace-copy:' || $1, 0))`

// lockWorkspaceCopies takes issue's copy lock for the rest of tx. Every write of a copy — both
// statements of a running copy's save (PutRunningWorkspaceCopy), a release's save
// (PutWorkspaceBundle), the keep of a confirmed restore (KeepRestoredRunningWorkspaceCopy,
// KeepRestoredWorkspaceBundle) and the cancel's move of a release bundle (CancelIssueRelease) —
// and every take (TakeWorkspaceCopyForRestore) and drop (DropHeldWorkspaceCopies) takes it as its
// first act, and stamps its write time with clock_timestamp() read once it holds the lock, never
// with the transaction's now(), which can be from before the wait. A copy's `at` is read before
// this lock, when its take began and before its bundler read the workspace (BeginWorkspaceCopy):
// it says what the work is as of, and orders no write against an ask, which is what the write
// time is for.
//
// The lock is held until the transaction commits, so of a write and a take of one issue, the one
// that holds it first has stamped its time and committed before the other stamps its own: a copy
// written before a pod's first ask is committed before the take reads the copies, and is read and
// offered at that ask; a copy written after it carries a later write time on the one clock, and is
// left for the next pod (runtime.DecideWorkspaceRestore), whenever its take began. Without it a
// write whose statement began before the ask and committed after the take had read would carry a
// write time earlier than the ask, unseen by it, and the pod's next ask would be sent that copy
// over the work it had done since.
//
// Each transaction that takes it takes it before it locks any row of either table, so the row
// locks they then take wait in one order. A delete writes no time, so the deletes a write of
// another copy makes take the rows alone (deleteRunningCopy); the cadence's own delete takes the
// lock, since it weighs the row's time against its take's and must read and delete under one
// (DeleteRunningWorkspaceCopy).
func lockWorkspaceCopies(ctx context.Context, tx pgx.Tx, issue string) error {
	if _, err := tx.Exec(ctx, workspaceCopyLock, issue); err != nil {
		return fmt.Errorf("take the workspace copy lock of %s: %w", issue, err)
	}
	return nil
}

// maxWorkspaceRestoreAsks is how many pods' first asks each issue keeps (firstRestoreAsk). An
// issue has one pod at a time, and two while one replaces the other, so a pod whose record goes
// has long been gone; a pod whose record went and that asked again would be recorded anew, at that
// ask, and could then be sent a copy written since its true first ask.
const maxWorkspaceRestoreAsks = 16

// firstRestoreAsk is when the pod of podUID first asked for a restore of issue's saved workspace
// copies, by the store's clock, recording this ask as that first one, at clock_timestamp() under
// the issue's copy lock, when the pod has asked before in no transaction that committed. A later
// ask of the same pod leaves the record as it is, so the time never moves (migrations'
// 20261114090103_workspace_restore_asks): its insert does nothing and answers no row, and the time
// is read in a statement of its own. A first ask also drops all but the issue's newest
// maxWorkspaceRestoreAsks records. tx holds the issue's copy lock (lockWorkspaceCopies).
func firstRestoreAsk(ctx context.Context, tx pgx.Tx, issue, podUID string) (time.Time, error) {
	var first time.Time
	err := tx.QueryRow(ctx, `insert into workspace_restore_asks (issue, pod_uid, first_asked_at) values ($1, $2, clock_timestamp())
		on conflict (issue, pod_uid) do nothing returning first_asked_at`, issue, podUID).Scan(&first)
	if errors.Is(err, pgx.ErrNoRows) {
		if err := tx.QueryRow(ctx, `select first_asked_at from workspace_restore_asks where issue = $1 and pod_uid = $2`,
			issue, podUID).Scan(&first); err != nil {
			return time.Time{}, fmt.Errorf("read when pod %s first asked for a restore: %w", podUID, err)
		}
		return first, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("record the first ask of pod %s for a restore: %w", podUID, err)
	}
	if _, err := tx.Exec(ctx, `delete from workspace_restore_asks where issue = $1 and pod_uid not in (
		select pod_uid from workspace_restore_asks where issue = $1 order by first_asked_at desc, pod_uid desc limit $2)`,
		issue, maxWorkspaceRestoreAsks); err != nil {
		return time.Time{}, fmt.Errorf("bound the recorded restore asks of %s: %w", issue, err)
	}
	return first, nil
}

// RecordPodRegistration records that a role of the pod of podUID has registered with the daemon,
// which spends every copy of issue's workspace that was sent to that pod (heldCopy): from the
// registration on a role runs there and may change the workspace, and a second restore of the
// copy there would move that work back. Until then the copy stays unspent, so a pod, or its roles
// container, that ends before any role of it registered leaves the copy for the next start of the
// issue: that pod's restarted container, whose launcher holds no record of the restore it ran and
// whose workspace no role has touched since, or the pod after it.
//
// The daemon calls it before it answers a registration of a role in that pod, the first and every
// one after it (internal/supervise's register and reregister, through Deps.PodRegistered), so no
// role is told it registered while the store still reads the copy unspent; a call that fails
// refuses the registration, and the agent's registration deadline relaunches it. Refusing is the
// safe side: a registration answered with no record would leave the copy unspent, so it could be
// sent again to a pod where a role has worked, which is the one send this record exists to stop,
// while a store fault that refuses the registration costs one launch failure. The record is the
// pod's: the first registration's time stays, and nothing a later role start or a restarted
// container does clears it (firstRestoreAsk leaves the row as it is).
//
// It takes the issue's copy lock first (lockWorkspaceCopies), as the take does before it reads the
// row, so a take and a registration of one pod are ordered: a take that held the lock first sends
// what it read and commits before the registration is written, and one that comes after reads the
// registration and sends nothing. It only updates: a pod with no row is left with none. Every
// pod's first role start asks for a restore before any role of it can register, so a missing row
// is one pruned (maxWorkspaceRestoreAsks) or one whose take failed, and either reads as spent.
func (s *Store) RecordPodRegistration(ctx context.Context, issue, podUID string) error {
	if podUID == "" {
		return fmt.Errorf("record a registration in a pod of %s: no pod named", issue)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkspaceCopies(ctx, tx, issue); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update workspace_restore_asks set registered_at = coalesce(registered_at, clock_timestamp())
			where issue = $1 and pod_uid = $2`, issue, podUID)
		return err
	})
	if err != nil {
		return fmt.Errorf("record a registration in pod %s of %s: %w", podUID, issue, err)
	}
	return nil
}

// heldCopy is what issue's copy in copies' table says of itself to the restore's decision — when
// its take began and when the store wrote it, whether it is spent and the pod its work came off —
// and the zero value when the issue holds none, the row locked for the rest of the restore's
// transaction. Every row names its pod and both its times: source_pod_uid is `not null` in both
// tables (migrations' 20261103214558_workspace_bundle_pod_binding and
// 20261112105448_workspace_running_copies), and so is written_at
// (20261114090103_workspace_restore_asks), as at always was.
//
// A copy whose attempt no take has recorded is unspent, and so is a copy sent to a pod where no
// role has registered: the row of workspace_restore_asks for the issue and the attempt's pod
// stands with registered_at null (RecordPodRegistration). Such a pod's roles ran nothing, so its
// restarted container and the pod after it are sent the copy again, and the decision is made
// again for that ask, every rule of it (runtime.DecideWorkspaceRestore); a send records the new
// pod (takeCopy). Every other attempt spends the copy: a refusal and a supersession, a send to a
// pod where a role registered, and a send to a pod whose row is gone.
func heldCopy(ctx context.Context, tx pgx.Tx, copies copyTable, issue string) (runtime.WorkspaceCopyHeld, error) {
	var held runtime.WorkspaceCopyHeld
	err := tx.QueryRow(ctx, fmt.Sprintf(`select c.at, c.written_at,
			c.attempt_pod_uid is not null and not (c.attempt_outcome = '%[2]s' and exists (select 1 from workspace_restore_asks a
				where a.issue = c.issue and a.pod_uid = c.attempt_pod_uid and a.registered_at is null)),
			c.source_pod_uid
		from %[1]s c where c.issue = $1 for update of c`, copies.table, sentCopyOutcome), issue).Scan(&held.At, &held.Written, &held.Spent, &held.Source)
	if errors.Is(err, pgx.ErrNoRows) {
		return runtime.WorkspaceCopyHeld{}, nil
	}
	if err != nil {
		return runtime.WorkspaceCopyHeld{}, fmt.Errorf("read the %s: %w", copies.kind, err)
	}
	return held, nil
}

// takeCopy records on issue's copy in copies' table the take the decision answered for it on the
// pod of podUID (runtime.DecideWorkspaceRestore), and answers the row as the take read it back. A
// refusal and a supersession spend the copy; a send spends it once a role of that pod registers
// (heldCopy), and a send of a copy sent before to a pod where no role registered moves the attempt
// to this pod. The row was read for update in this same transaction (heldCopy), so a take that
// finds none is an error like any other. The body is read back only when it is being sent.
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

// DeleteRunningWorkspaceCopy drops issue's running copy where its take began at or before begun,
// the time the take of the deleting copy began (BeginWorkspaceCopy). It is the cadence's own
// delete, of a workspace that bundled to nothing: a copy taken minutes before holds work that has
// since been pushed, and the next pod must never restore it. The two writes that invalidate what
// the issue holds drop the copy inside their own transaction, with the release bundle each saves
// or drops (PutWorkspaceBundle, DeleteWorkspaceBundle), and those deletes read no time: the
// bundle they write or drop is the issue's whole answer either way. The drop of the copies a kept
// volume holds deletes in a transaction of its own too (DropHeldWorkspaceCopies), each copy by the
// time its take began against the Sandbox's creation.
//
// A row whose take began after this take is left standing and answers ErrNewerCopyStands, which
// is the rule a save of a copy keeps (PutRunningWorkspaceCopy): a read that found nothing
// unpushed says what the workspace held as the take began, and nothing of the work a later copy
// read. The row this delete meets is one written at a move — a cancelled release's
// (CancelIssueRelease) or a confirmed restore's (KeepRestoredWorkspaceBundle,
// KeepRestoredRunningWorkspaceCopy) — or a copy off another pod, and each holds unpushed work
// this delete would drop. The read and the delete are one transaction under the issue's copy
// lock (lockWorkspaceCopies), so no copy is written between them. Deleting a copy the table does
// not hold is success, so a retried delete finishes, and a delete handed no take's begin is
// refused: a delete the store cannot order is one it cannot keep this rule for.
//
// project is the token of the daemon whose cadence took the empty bundle (claim.ProjectToken),
// and the delete drops only where project records issue (projectRecordsIssue), the rule the save
// keeps (PutRunningWorkspaceCopy). Where it does not, which the delete reads under the issue's
// copy lock before it reads the row, it drops nothing and answers ErrIssueNotRecorded: the
// cadence copies an operator's spawn of an issue another project records as it copies every claim
// of its own project (internal/daemon's runningCopyClaim), and the spawn's workspace says nothing
// of the work of the project that records the issue, so its empty bundle would delete that
// project's running copy, the work that project's next pod is to be sent. An empty project is
// refused, as the take refuses one.
func (s *Store) DeleteRunningWorkspaceCopy(ctx context.Context, project, issue string, begun WorkspaceCopyBegun) error {
	if project == "" {
		return fmt.Errorf("the delete of the running workspace copy of %s needs the project whose cadence took it", issue)
	}
	if begun.at.IsZero() {
		return fmt.Errorf("the delete of the running workspace copy of %s needs the store's time its take began (BeginWorkspaceCopy)", issue)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkspaceCopies(ctx, tx, issue); err != nil {
			return err
		}
		if recorded, err := projectRecordsIssue(ctx, tx, project, issue); err != nil {
			return err
		} else if !recorded {
			return fmt.Errorf("project %s: %w", project, ErrIssueNotRecorded)
		}
		dropped, err := tx.Exec(ctx, `delete from workspace_running_copies where issue = $1 and at <= $2`, issue, begun.at)
		if err != nil || dropped.RowsAffected() > 0 {
			return err
		}
		var held bool
		if err := tx.QueryRow(ctx, `select exists (select 1 from workspace_running_copies where issue = $1)`,
			issue).Scan(&held); err != nil {
			return err
		}
		if held {
			return fmt.Errorf("taken at %s: %w", begun.at, ErrNewerCopyStands)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("delete the running workspace copy of %s: %w", issue, err)
	}
	return nil
}

// deleteRunningCopy drops issue's running copy whatever minute took it, through db: the
// transaction of each write that drops the copy with what it changes (PutWorkspaceBundle,
// DeleteWorkspaceBundle), each of which leaves the issue an answer of its own and so reads no
// time. The cadence's own delete reads one (DeleteRunningWorkspaceCopy). Deleting one the table
// does not hold is success.
func deleteRunningCopy(ctx context.Context, db execer, issue string) error {
	_, err := db.Exec(ctx, `delete from workspace_running_copies where issue = $1`, issue)
	return err
}

// KeepRestoredRunningWorkspaceCopy keeps issue's running copy of restoreID, which the launcher of
// the pod of podUID has reported restored, as that pod's own copy of the work: under the issue's
// copy lock (lockWorkspaceCopies), in one statement, it names that pod as the row's source, gives
// the row a restore id of its own and clears its attempt (runningCopyReplaced), leaving the work as
// it is.
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
// transaction (PutWorkspaceBundle), and a copy of that pod that finds nothing unpushed drops it
// where its own take began after the confirmation (DeleteRunningWorkspaceCopy).
//
// Both of the kept copy's times, when its take began and when it was written, are the store's
// clock read once under that lock rather than the copy's own, for the reason a kept release
// bundle's are (KeepRestoredWorkspaceBundle): the row stands for the restoring pod's workspace as
// it is once the restore is confirmed, which is when that pod's copy of the work begins, and under
// the old time a push recorded after it would refuse the kept copy to the next pod
// (runtime.WorkspaceBundleRefusedPushed) and lose the work again. A push recorded after the
// confirmation takes a copy of its own off the restoring pod's live roles, so the push rule keeps
// its meaning. The next pod is made once the restoring pod has ended, which is after the
// confirmation, so its first ask comes after the kept copy's write and it is sent the work.
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
// It keeps only a copy whose attempt is a send to that pod: the row's attempt names podUID and
// records the send (attempt_outcome, TakeWorkspaceCopyForRestore). Any other launcher of the
// issue — one whose pod the take answered nothing, refused the copy or closed it superseded, or
// one whose pod ended before any role of it registered and the copy was sent again to another —
// reports work the row's attempt does not name, and its report changes nothing, so the row is
// never sourced to a pod whose disk does not hold the work, and the attempt stays as the take left
// it.
func (s *Store) KeepRestoredRunningWorkspaceCopy(ctx context.Context, issue, podUID string, restoreID int64) error {
	if podUID == "" {
		return fmt.Errorf("the restored running workspace copy %d of %s needs the pod that restored it", restoreID, issue)
	}
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockWorkspaceCopies(ctx, tx, issue); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update workspace_running_copies
			set source_pod_uid = $3, at = stamp.clock, written_at = stamp.clock, `+runningCopyReplaced+`
			from (select clock_timestamp() as clock) stamp
			where issue = $1 and restore_id = $2 and attempt_pod_uid = $3 and attempt_outcome = $4`,
			issue, restoreID, podUID, sentCopyOutcome)
		return err
	})
	if err != nil {
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
