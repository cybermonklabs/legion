package store

import (
	"context"
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// queueEventsVersion is the version of the migration this file tests, so the test can migrate
// through everything before it whatever lands beside it.
const queueEventsVersion = 20261126020201

// queueEventsRelease names the release whose way back the migration's header carries, as a failure
// to read that block names it.
const queueEventsRelease = "the release that keeps each merge-queue timeline event of a project's pull requests"

// 20261126020201 adds queue_events, one row per GitHub timeline event of a project's pull
// requests' merge-queue moves, queue_progress, how many entries and exits of each pull request the
// daemon has kept, which the 6-hour delete of queue_events leaves, and adds read_error on
// signal_inputs so a failed boot read marks the queue input absent with its reason until a read
// succeeds. Its way back drops both tables, deletes queue_timeline_read rows (the kind
// 20261126190955 admits), drops the added column, deletes the queue row of signal_inputs and
// deletes this version row so a roll-forward runs the file again. The file writes no list of the
// outbox's kind check, so the roll forward succeeds over a merge_holds_changed row the earlier
// release queued while it ran: a list written again here, which could not name that kind, would
// refuse the row and stop Migrate (SQLSTATE 23514).
func TestTheQueueEventsArriveWith20261126020201AndTheirWayBackLetsThemComeAgain(t *testing.T) {
	ctx := context.Background()
	store := emptyStore(t)
	migrateThrough(t, store, queueEventsVersion-1)
	exec := func(statement string, args ...any) error {
		_, err := store.Pool().Exec(ctx, statement, args...)
		return err
	}
	const tables = `select count(*)::text from information_schema.tables where table_name in ('queue_events', 'queue_progress')`
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"0"}) {
		t.Fatalf("queue_events and queue_progress before the migration = %v, want both absent", got)
	}

	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate through queue events: %v", err)
	}
	const insertEvent = `insert into queue_events (project, event_id, repo, pull_request, at, enters, position) values ($1, $2, $3, $4, $5, $6, $7)`
	for _, kept := range []struct {
		project, repo, at string
		id                int64
		pr, pos           int
		enters            bool
	}{
		{"LEGION", "cybermonklabs/legion", "2026-11-23T02:27:46Z", 36963393320, 5150, 0, true},
		{"LEGION", "cybermonklabs/legion", "2026-11-23T18:25:55Z", 37043365149, 5150, 1, false},
		{"ACME", "cybermonklabs/legion", "2026-11-23T02:27:46Z", 36963393320, 5150, 0, true},
	} {
		if err := exec(insertEvent, kept.project, kept.id, kept.repo, kept.pr, kept.at, kept.enters, kept.pos); err != nil {
			t.Fatalf("insert valid queue event %+v: %v", kept, err)
		}
	}
	for _, refused := range []struct {
		name, project, repo string
		id                  int64
		pr, pos             int
	}{
		{"duplicate id in same project", "LEGION", "cybermonklabs/legion", 36963393320, 5150, 0},
		{"empty project", "", "cybermonklabs/legion", 99, 5150, 0},
		{"non-positive event id", "LEGION", "cybermonklabs/legion", 0, 5150, 0},
		{"empty repo", "LEGION", "", 100, 5150, 0},
		{"non-positive pull request", "LEGION", "cybermonklabs/legion", 101, 0, 0},
		{"negative position", "LEGION", "cybermonklabs/legion", 102, 5150, -1},
	} {
		if err := exec(insertEvent, refused.project, refused.id, refused.repo, refused.pr, "2026-11-23T02:27:46Z", true, refused.pos); err == nil {
			t.Errorf("%s was accepted, want it refused", refused.name)
		}
	}
	const insertProgress = `insert into queue_progress (project, repo, pull_request, entered, exited) values ($1, $2, $3, $4, $5)`
	if err := exec(insertProgress, "LEGION", "cybermonklabs/legion", 5150, 1, 2); err != nil {
		t.Fatalf("insert valid queue progress: %v", err)
	}
	for _, refused := range []struct {
		name, project, repo string
		pr, entered, exited int
	}{
		{"the same pull request under the same name", "LEGION", "cybermonklabs/legion", 5150, 2, 2},
		{"empty project", "", "cybermonklabs/legion", 5150, 1, 0},
		{"empty repo", "LEGION", "", 5150, 1, 0},
		{"non-positive pull request", "LEGION", "cybermonklabs/legion", 0, 1, 0},
		{"negative entries", "LEGION", "cybermonklabs/legion", 5151, -1, 0},
		{"negative exits", "LEGION", "cybermonklabs/legion", 5151, 0, -1},
	} {
		if err := exec(insertProgress, refused.project, refused.repo, refused.pr, refused.entered, refused.exited); err == nil {
			t.Errorf("queue progress of %s was accepted, want it refused", refused.name)
		}
	}
	if err := exec(insertProgress, "LEGION", "sjawhar/legion", 5150, 1, 0); err != nil {
		t.Fatalf("insert the queue progress of the pull request under the name a rename pairs: %v", err)
	}
	if err := exec(`insert into signal_inputs (project, input, kept_since, read_error) values ('LEGION', 'queue', '2026-11-26T01:00:00Z', 'boot read failed')`); err != nil {
		t.Fatalf("insert signal_inputs with read_error: %v", err)
	}
	if err := exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ($1, 'LEGION-208', '{}', 0, now(), '')`, string(record.OutboxKindQueueTimelineRead)); err != nil {
		t.Fatalf("queue queue_timeline_read after migration: %v", err)
	}
	const timelineReads = `select count(*)::text from outbox where kind = 'queue_timeline_read'`

	if err := exec(wayBackOf(t, queueEventsVersion, queueEventsRelease)); err != nil {
		t.Fatalf("the way back: %v", err)
	}
	const version = `select count(*)::text from schema_version where version = 20261126020201`
	if got := append(append(rowsAsText(t, store, tables), rowsAsText(t, store, version)...), rowsAsText(t, store, timelineReads)...); !reflect.DeepEqual(got, []string{"0", "0", "0"}) {
		t.Fatalf("after the way back queue_events and queue_progress, version row and queue_timeline_read rows = %v, want none of them", got)
	}
	if err := exec(`insert into outbox (kind, issue, payload, attempts, next_at, last_error) values ('merge_holds_changed', 'LEGION-208', '{}', 0, now(), '')`); err != nil {
		t.Fatalf("queue the earlier release's merge_holds_changed row after the way back: %v", err)
	}
	if _, err := store.Migrate(ctx); err != nil {
		t.Fatalf("roll forward after the way back: %v", err)
	}
	if got := rowsAsText(t, store, tables); !reflect.DeepEqual(got, []string{"2"}) {
		t.Fatalf("queue_events and queue_progress after roll-forward = %v, want both present again", got)
	}
}
