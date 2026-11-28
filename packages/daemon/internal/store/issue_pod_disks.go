package store

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// StartKeptPod is a role's start in the pod of podUID of issue, whose Sandbox owns the issue's kept
// volume and was created at keptSince, as the store keeps it. project is the token of the daemon
// whose pod asks (claim.ProjectToken). It records the pod with its kept volume for the issue's
// update round (issue_pod_disks), in a transaction of its own.
//
// A kept volume outlives its pod: an eviction, a launch's replacement, an update round's
// replacement and an idle release keep it for the issue's next pod, until a reset mark or a change
// of the disk's shape replaces the Sandbox with its volume. So a pod that mounts a kept volume an
// earlier pod of the issue mounted holds what the roles of that earlier pod left unpushed, and the
// record marks the pod as holding work from before it (holds_earlier_work) where another pod of
// the issue has a row with the same keptSince, a pod of the same volume.
//
// The launcher calls it at every role's start in a pod whose Sandbox owns a kept volume, and starts
// no role where it fails (sandbox's launchers.recordKeptPod, runtime.KeptPodUnrecorded), so the
// store holds each pod of a kept volume of the project's issues in which a role started. It writes
// nothing where project does not record issue (projectRecordsIssue): an issue another project
// records is that project's daemon's on a database the two share, and its pods are that daemon's
// to record, and an issue no project records is no daemon's.
//
// The mark is decided here, at the write, and not at the update round's read. A Sandbox runs one
// pod at a time, so at a pod's first record every other row of its volume is a pod that came
// before it; the round reads a pod at an end that can come after the next pod of the same volume
// recorded (a launch's replacement, a pod that went), and a mark read then from the rows would mark
// the first pod of a volume for the later one. A later pod of a volume never marks an earlier one,
// so the first pod of a volume this release records is never marked.
//
// A pod's first record owns its row, kept_since and mark alike: a pod's Sandbox, and with it the
// volume, is the same for the pod's whole life, and the mark is decided at that first record
// (above). A later start in a recorded pod writes nothing, so no write changes a mark or clears
// one.
//
// It refuses an empty project, an empty podUID and a zero keptSince: the record could say neither
// whose pod it is, nor which pod, nor which volume. It runs until one run commits
// (transactUntilCommitted): a run after one whose commit reply was lost finds the row and writes
// nothing.
func (s *Store) StartKeptPod(ctx context.Context, project, issue, podUID string, keptSince time.Time) error {
	switch {
	case project == "":
		return fmt.Errorf("the record of a pod of the kept volume of %s needs the project whose pod asks", issue)
	case podUID == "":
		return fmt.Errorf("the record of a pod of the kept volume of %s needs the pod that mounts it", issue)
	case keptSince.IsZero():
		return fmt.Errorf("the record of a pod of the kept volume of %s needs when the Sandbox of the pod %s was created", issue, podUID)
	}
	err := transactUntilCommitted(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `insert into issue_pod_disks (issue, pod_uid, kept_since, holds_earlier_work)
			select $1, $2, $3, exists (select 1 from issue_pod_disks where issue = $1 and pod_uid <> $2 and kept_since = $3)
			where `+projectRecordsIssue+`
			on conflict (issue, pod_uid) do nothing`,
			issue, podUID, keptSince, project)
		return err
	})
	if err != nil {
		return fmt.Errorf("record the pod %s of %s with its kept volume: %w", podUID, issue, err)
	}
	return nil
}

// projectRecordsIssue is whether project $4 records issue $1 (issues.project, the Dispatch key the
// project's token lowercases), which is what makes the record of the disks of the issue's pods that
// project's daemon's to write (StartKeptPod).
const projectRecordsIssue = `exists (select 1 from issues i where i.key = $1 and lower(i.project) = $4)`

// PodHoldsEarlierWork is whether the store marks the pod of podUID of issue as a pod whose disk
// holds work from before the pod (issue_pod_disks.holds_earlier_work): it mounts a kept volume that
// an earlier pod of the issue mounted (StartKeptPod). A pod the store does not record is not
// marked. internal/daemon's update round reads it for whom each end of a round over the pod names
// (workOnRoundPod). The mark stands until the issue's work is over (DeleteIssuePodDisks).
func (s *Store) PodHoldsEarlierWork(ctx context.Context, issue, podUID string) (bool, error) {
	var holds bool
	err := TransactWith(ctx, s.pool, pgx.TxOptions{AccessMode: pgx.ReadOnly}, func(ctx context.Context, tx pgx.Tx) error {
		return tx.QueryRow(ctx, `select exists (select 1 from issue_pod_disks
			where issue = $1 and pod_uid = $2 and holds_earlier_work)`, issue, podUID).Scan(&holds)
	})
	if err != nil {
		return false, fmt.Errorf("read whether the disk of the pod %s of %s holds work from before the pod: %w", podUID, issue, err)
	}
	return holds, nil
}

// DeleteIssuePodDisks drops the record of the disk of every pod of issue (issue_pod_disks): the
// issue's work is over, so no round of its pods is left to read it (outbox's forgetIdleRelease).
// Deleting rows the table does not hold is success, so a retried delete finishes.
func (s *Store) DeleteIssuePodDisks(ctx context.Context, issue string) error {
	err := Transact(ctx, s.pool, func(ctx context.Context, tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `delete from issue_pod_disks where issue = $1`, issue)
		return err
	})
	if err != nil {
		return fmt.Errorf("delete the record of the disks of the pods of %s: %w", issue, err)
	}
	return nil
}
