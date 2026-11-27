package textversionstest

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store"
	"github.com/sjawhar/envoy/internal/testwait"
)

// RowsOf counts the keyword table's rows of the events ids.
func RowsOf(t testing.TB, database *store.Store, ids ...int64) int {
	t.Helper()
	var count int
	if err := database.Pool.QueryRow(t.Context(), `select count(*) from search_text_versions where event_id = any($1)`, ids).Scan(&count); err != nil {
		t.Fatalf("count the rows of events %v: %v", ids, err)
	}
	return count
}

// RequireRowsOf holds each event of want to the rows it starts: as many as its Versions, and none
// for one that starts none. It reads the rows of no other event, so a test whose other writes
// have rows of their own holds its events alone; RequireOnlyRowsOf holds the rest of the table
// too.
func RequireRowsOf(t testing.TB, database *store.Store, want []Written) {
	t.Helper()
	requireRows(t, database, want, false)
}

// RequireOnlyRowsOf holds the keyword table to exactly the rows of want: each event as many rows
// as it starts, and no row of any other event.
func RequireOnlyRowsOf(t testing.TB, database *store.Store, want []Written) {
	t.Helper()
	requireRows(t, database, want, true)
}

// requireRows holds each event of want to the rows it starts and, with only, every other event of
// the table to none.
func requireRows(t testing.TB, database *store.Store, want []Written, only bool) {
	t.Helper()
	expected := make(map[int64]int, len(want))
	ids := make([]int64, 0, len(want))
	for _, w := range want {
		expected[w.ID] = w.Versions
		ids = append(ids, w.ID)
	}
	query, args := `select event_id, count(*) from search_text_versions where event_id = any($1) group by event_id`, []any{ids}
	if only {
		query, args = `select event_id, count(*) from search_text_versions group by event_id`, nil
	}
	rows, err := database.Pool.Query(t.Context(), query, args...)
	if err != nil {
		t.Fatalf("count the rows of each event: %v", err)
	}
	got := map[int64]int{}
	for rows.Next() {
		var id int64
		var count int
		if err := rows.Scan(&id, &count); err != nil {
			t.Fatalf("count the rows of each event: %v", err)
		}
		got[id] = count
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("count the rows of each event: %v", err)
	}
	for id := range got {
		if _, ok := expected[id]; !ok {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	var wrong []string
	for _, id := range ids {
		if got[id] != expected[id] {
			wrong = append(wrong, fmt.Sprintf("event %d: %d rows, want %d", id, got[id], expected[id]))
		}
	}
	if len(wrong) > 0 {
		t.Fatalf("%d events have the wrong rows, the first: %v", len(wrong), wrong[:min(len(wrong), 10)])
	}
}

// AwaitRowsUntil waits until every event of ids has its one row, running each between reads when
// it is set, bounded by the test binary's deadline alone (testwait.UntilPolling), whose failure
// names the events and the count of rows it read last. The fill writes an event's row only once
// the event horizon has passed the event, and the horizon waits on every event write of the
// database open when it observes (events.Horizon, "Costs"), which a loaded machine can keep open
// for seconds: a fixed span would measure how long such a write was held, not the fill.
func AwaitRowsUntil(t *testing.T, database *store.Store, ids []int64, each func()) {
	t.Helper()
	testwait.UntilPolling(t, 50*time.Millisecond, fmt.Sprintf("one row of each of the events %v, the rows counted", ids), func() (int, bool) {
		rows := RowsOf(t, database, ids...)
		if rows == len(ids) {
			return rows, true
		}
		if each != nil {
			each()
		}
		return rows, false
	})
}

// Cursor is the fill's cursor, the event its cursor row names, and 0 before the fill wrote the
// row.
func Cursor(t testing.TB, database *store.Store) int64 {
	t.Helper()
	var cursor int64
	if err := database.Pool.QueryRow(t.Context(),
		`select coalesce((select event_id from search_text_versions_fill where id = 1), 0)`).Scan(&cursor); err != nil {
		t.Fatalf("read the keyword fill's cursor: %v", err)
	}
	return cursor
}
