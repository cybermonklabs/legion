package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// VolumeNoticeCounts is what the daemon's bounds on volume-near-full notices read before it
// writes one (pod_volume_notices): how many it has already written for this trimmer and episode,
// and when it last wrote one for this pod under any trimmer or episode.
//
// The two are read together, in one statement and one transaction with the write that follows
// them, because they bound different things and a reader that took them apart could act on a
// count and an hour from either side of another daemon's write. Notices counts the line the
// three-notice bound holds; LastAt is the pod's own newest notice, which is what the hour holds,
// and the zero time is a pod nothing has been written for.
type VolumeNoticeCounts struct {
	Notices int
	LastAt  time.Time
}

// VolumeNoticeCounts reads the bounds of one report inside the caller's transaction: the notices
// already written for issue's pod under this trimmer and episode, and the newest notice of that
// pod whatever line it was on. The engine decides from them and writes through
// RecordVolumeNotice in the same transaction, so the count a notice is written against is the
// count it is counted into.
//
// A trimmer identifier the daemon has not seen reads zero notices, which is what starts a
// restarted roles container's count over: the identifier is minted when the launcher process
// starts, and it is part of the key.
func (p *PodRecords) VolumeNoticeCounts(ctx context.Context, tx pgx.Tx, issue, podUID, trimmer string, episode int) (VolumeNoticeCounts, error) {
	if issue == "" || podUID == "" {
		return VolumeNoticeCounts{}, errors.New("the bounds of a volume notice need its issue and pod")
	}
	var counts VolumeNoticeCounts
	var notices *int64
	var last *time.Time
	err := tx.QueryRow(ctx, `select
		(select notices from pod_volume_notices
			where project = $1 and issue = $2 and pod_uid = $3 and trimmer = $4 and episode = $5),
		(select max(at) from pod_volume_notices
			where project = $1 and issue = $2 and pod_uid = $3)`,
		p.project, issue, podUID, trimmer, int64(episode)).Scan(&notices, &last)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return VolumeNoticeCounts{}, fmt.Errorf("read the volume notice bounds of %s: %w", issue, err)
	}
	if notices != nil {
		counts.Notices = int(*notices)
	}
	if last != nil {
		counts.LastAt = last.UTC()
	}
	return counts, nil
}

// RecordVolumeNotice counts one volume-near-full notice of issue's pod against the line it was
// written on — this project, issue, pod uid, trimmer and episode — inside the caller's
// transaction, and answers the count that line now stands at. The notice's own outbox row is
// written in that same transaction, so a notice held for an architect that is not there has
// still spent its count, and a transaction that rolls back has spent none.
//
// greatest keeps At the newest notice, so a write made out of order never moves the line's end
// backwards, as RecordPodHold keeps a hold streak's end.
func (p *PodRecords) RecordVolumeNotice(ctx context.Context, tx pgx.Tx, issue, podUID, trimmer string, episode int, at time.Time) (int, error) {
	if issue == "" || podUID == "" {
		return 0, errors.New("a volume notice record needs its issue and pod")
	}
	var notices int64
	err := tx.QueryRow(ctx, `insert into pod_volume_notices (project, issue, pod_uid, trimmer, episode, notices, first_at, at)
		values ($1, $2, $3, $4, $5, 1, $6, $6)
		on conflict (project, issue, pod_uid, trimmer, episode) do update
		set notices = pod_volume_notices.notices + 1, at = greatest(pod_volume_notices.at, excluded.at)
		returning notices`,
		p.project, issue, podUID, trimmer, int64(episode), at).Scan(&notices)
	if err != nil {
		return 0, fmt.Errorf("record the volume notice of %s: %w", issue, err)
	}
	return int(notices), nil
}
