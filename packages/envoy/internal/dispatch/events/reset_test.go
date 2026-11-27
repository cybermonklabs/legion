package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// liveEventSeed is one event a test writes directly, oldest first.
type liveEventSeed struct {
	kind    string
	actor   string
	payload string
	at      time.Time
}

// seedLiveEvents writes each event on issue LIVE-1, oldest first, and returns the ids they took.
func seedLiveEvents(t *testing.T, database *store.Store, seeds []liveEventSeed) []int64 {
	t.Helper()
	ctx := context.Background()
	for _, statement := range []string{
		`insert into projects (key, name, default_assignee) values ('LIVE', 'Live', 'alice') on conflict do nothing`,
		`insert into issues (key, project_key, number, title, created_by, rank) values ('LIVE-1', 'LIVE', 1, 'x', '{"kind":"user","id":"alice"}', 'U') on conflict do nothing`,
	} {
		if _, err := database.Pool.Exec(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	ids := make([]int64, 0, len(seeds))
	for _, seed := range seeds {
		var id int64
		if err := database.Pool.QueryRow(ctx, `
			insert into events (issue_key, seq, type, actor, payload, notify, created_at)
			select 'LIVE-1', coalesce(max(seq), 0) + 1, $1, $2::jsonb, $3::jsonb, false, $4
			from events where issue_key = 'LIVE-1'
			returning id
		`, seed.kind, seed.actor, seed.payload, seed.at).Scan(&id); err != nil {
			t.Fatalf("insert %s event: %v", seed.kind, err)
		}
		ids = append(ids, id)
	}
	return ids
}

// liveUser is the actor of the events a test writes.
const liveUser = `{"kind":"user","id":"alice"}`

// EventCursorCheck reads the newest event's id and the time of the event at the cursor; once the
// cursor's row is gone (a reset of the log), cursorAt is nil.
func TestEventCursorCheckReadsTheNewestEventAndTheCursorsRow(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()

	maxID, cursorAt, err := EventCursorCheck(ctx, database.Pool, 0)
	if err != nil {
		t.Fatalf("EventCursorCheck on an empty log: %v", err)
	}
	if maxID != 0 || cursorAt != nil {
		t.Fatalf("an empty log = %d, %v; want 0, nil", maxID, cursorAt)
	}

	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "artifact.version", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	maxID, cursorAt, err = EventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("EventCursorCheck: %v", err)
	}
	if maxID != ids[2] {
		t.Fatalf("newest event = %d, want %d", maxID, ids[2])
	}
	if cursorAt == nil || !cursorAt.Equal(base.Add(time.Minute)) {
		t.Fatalf("the cursor's row time = %v, want %v", cursorAt, base.Add(time.Minute))
	}

	if _, err := database.Pool.Exec(ctx, `delete from events where id = $1`, ids[1]); err != nil {
		t.Fatalf("delete the cursor's row: %v", err)
	}
	maxID, cursorAt, err = EventCursorCheck(ctx, database.Pool, ids[1])
	if err != nil {
		t.Fatalf("EventCursorCheck once the cursor's row is gone: %v", err)
	}
	if maxID != ids[2] || cursorAt != nil {
		t.Fatalf("with the cursor's row gone = %d, %v; want %d, nil", maxID, cursorAt, ids[2])
	}

	if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
		t.Fatalf("reset the event log: %v", err)
	}
	reset := seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	maxID, cursorAt, err = EventCursorCheck(ctx, database.Pool, ids[2])
	if err != nil {
		t.Fatalf("EventCursorCheck after a reset: %v", err)
	}
	if maxID != reset[0] || maxID >= ids[2] || cursorAt != nil {
		t.Fatalf("after a reset = %d, %v; want the new newest id %d below the cursor %d, and nil", maxID, cursorAt, reset[0], ids[2])
	}
}

// cursorCheck is one answer of EventCursorCheck.
type cursorCheck struct {
	maxID    int64
	cursorAt *time.Time
}

func (c cursorCheck) equal(other cursorCheck) bool {
	if c.maxID != other.maxID {
		return false
	}
	if c.cursorAt == nil || other.cursorAt == nil {
		return c.cursorAt == nil && other.cursorAt == nil
	}
	return c.cursorAt.Equal(*other.cursorAt)
}

func (c cursorCheck) String() string {
	cursorAt := "no row"
	if c.cursorAt != nil {
		cursorAt = c.cursorAt.UTC().Format(time.RFC3339)
	}
	return fmt.Sprintf("newest id %d, cursor's row %s", c.maxID, cursorAt)
}

// resetAfterFirstStatement is a queryer that resets the event log the moment the first statement
// read through it has answered, which is a reset committing in the middle of whatever read that
// statement began.
type resetAfterFirstStatement struct {
	Queryer
	reset func()
	fired bool
}

func (q *resetAfterFirstStatement) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return scanThen{row: q.Queryer.QueryRow(ctx, sql, args...), after: func() {
		if !q.fired {
			q.fired = true
			q.reset()
		}
	}}
}

// scanThen is a row that runs after once its Scan has answered.
type scanThen struct {
	row   pgx.Row
	after func()
}

func (r scanThen) Scan(dest ...any) error {
	err := r.row.Scan(dest...)
	r.after()
	return err
}

// A reset of the event log (here a `truncate events restart identity` and its refill; e2e/seed.ts
// deletes the rows and restarts the ids, which does the same) that
// commits while EventCursorCheck is reading must leave the answer describing one log, the old one
// or the new one. A mixed answer - the old log's newest id with no row at the cursor in the new
// one - is one the relay would take for a reset to the old log's head: it would move its cursor
// there, and the new log's lower ids would then read as a second reset that closed the
// subscription opened after the first (api's TestEventStreamRelayRecoversWhenTheEventLogIsReset).
func TestEventCursorCheckReadsOneLogWhenTheLogIsResetMidCheck(t *testing.T) {
	t.Parallel()
	database := storetest.Open(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ids := seedLiveEvents(t, database, []liveEventSeed{
		{kind: "issue.created", actor: liveUser, payload: `{}`, at: base},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(time.Minute)},
		{kind: "issue.updated", actor: liveUser, payload: `{}`, at: base.Add(2 * time.Minute)},
	})
	cursor := ids[1]
	check := func(q Queryer) cursorCheck {
		t.Helper()
		maxID, cursorAt, err := EventCursorCheck(ctx, q, cursor)
		if err != nil {
			t.Fatalf("EventCursorCheck: %v", err)
		}
		return cursorCheck{maxID: maxID, cursorAt: cursorAt}
	}

	oldLog := check(database.Pool)
	resetting := &resetAfterFirstStatement{Queryer: database.Pool, reset: func() {
		if _, err := database.Pool.Exec(ctx, `truncate events restart identity cascade`); err != nil {
			t.Fatalf("reset the event log: %v", err)
		}
		seedLiveEvents(t, database, []liveEventSeed{{kind: "issue.created", actor: liveUser, payload: `{}`, at: base.Add(time.Hour)}})
	}}
	got := check(resetting)
	if !resetting.fired {
		t.Fatal("the check read nothing through the queryer, so no reset landed inside it")
	}
	newLog := check(database.Pool)

	if !got.equal(oldLog) && !got.equal(newLog) {
		t.Fatalf("a reset during the check answered %v; want the old log's answer (%v) or the new log's (%v), not two logs in one answer",
			got, oldLog, newLog)
	}
}
