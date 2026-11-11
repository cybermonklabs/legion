package store

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// volumeNoticesVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const volumeNoticesVersion = 20261111090000

// 20261111090000 adds pod_volume_notices, the count of what an issue's architect has been told
// about the volume its pod's roles share filling under them. The negative control is the write of
// a count itself, which has no table to land in before the migration.
//
// What the shape must give, and what this drives after it: one counted row per project, issue,
// pod uid, trimmer and episode — a second write of the same line counts a notice rather than
// making a row, and another trimmer or another episode is a line of its own, which is what starts
// a restarted roles container's count over — and a read of the pod's newest notice whatever line
// wrote it, which is what the hour between one pod's notices is measured from.
func TestTheVolumeNoticeCountsArriveWith20261111090000(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, volumeNoticesVersion-1)

	// The negative control: before the migration there is no table to count in.
	const count = `insert into pod_volume_notices (project, issue, pod_uid, trimmer, episode, first_at, at)
		values ('legion', 'LEGION-208', 'pod-uid', 'trim-1', 1, now(), now())`
	if _, err := store.Pool().Exec(ctx, count); err == nil || !strings.Contains(err.Error(), "pod_volume_notices") {
		t.Fatalf("before the migration a volume notice count = %v, want the table to be missing", err)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through the volume notice counts: %v", err)
	}

	records, err := store.PodRecordsOf("legion")
	if err != nil {
		t.Fatalf("read the project's pod records: %v", err)
	}
	at := time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	record := func(trimmer string, episode int, when time.Time) int {
		t.Helper()
		var notices int
		if err := store.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			notices, err = records.RecordVolumeNotice(ctx, tx, "LEGION-208", "pod-uid", trimmer, episode, when)
			return err
		}); err != nil {
			t.Fatalf("count a notice of %s episode %d: %v", trimmer, episode, err)
		}
		return notices
	}
	read := func(trimmer string, episode int) VolumeNoticeCounts {
		t.Helper()
		var counts VolumeNoticeCounts
		if err := store.Tx(ctx, func(tx pgx.Tx) error {
			var err error
			counts, err = records.VolumeNoticeCounts(ctx, tx, "LEGION-208", "pod-uid", trimmer, episode)
			return err
		}); err != nil {
			t.Fatalf("read the counts of %s episode %d: %v", trimmer, episode, err)
		}
		return counts
	}

	if counts := read("trim-1", 1); counts.Notices != 0 || !counts.LastAt.IsZero() {
		t.Fatalf("a line nothing has written reads %+v, want no notices and no last", counts)
	}
	if got := record("trim-1", 1, at); got != 1 {
		t.Fatalf("the first notice of a line counts %d, want 1", got)
	}
	if got := record("trim-1", 1, at.Add(time.Hour)); got != 2 {
		t.Fatalf("the second notice of the same line counts %d, want 2: the key is what makes it one row", got)
	}
	if counts := read("trim-1", 1); counts.Notices != 2 || !counts.LastAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("the line reads %+v, want two notices last written an hour on", counts)
	}
	// A write out of order never moves the line's end backwards, as a hold streak's does not.
	if got := record("trim-1", 1, at.Add(-time.Hour)); got != 3 {
		t.Fatalf("a notice written out of order counts %d, want 3", got)
	}
	if counts := read("trim-1", 1); !counts.LastAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("the line's last notice is %s, want the newest write to stand", counts.LastAt)
	}

	// Another episode of the same trimmer, and another trimmer, are lines of their own; the pod's
	// newest notice is read across all of them, which is what the hour is measured from.
	if got := record("trim-1", 2, at.Add(2*time.Hour)); got != 1 {
		t.Fatalf("the first notice of another episode counts %d, want 1", got)
	}
	if got := record("trim-2", 1, at.Add(3*time.Hour)); got != 1 {
		t.Fatalf("the first notice of a new trimmer counts %d, want 1: a restarted roles container starts the count over", got)
	}
	if counts := read("trim-1", 1); counts.Notices != 3 || !counts.LastAt.Equal(at.Add(3*time.Hour)) {
		t.Fatalf("the first line reads %+v, want its own three notices and the pod's newest notice across every line", counts)
	}
}
