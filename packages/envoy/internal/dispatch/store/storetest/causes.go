package storetest

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store"
)

// RequireNoMarkedCause fails t when a committed graph_nodes row or graph_tuples row of database
// holds a negative source_event. A node write marks each row it makes or moves with a negative
// source_event until the statement that states that write's facts moves the row onto its cause
// (graphstore's nodeMark, graphstore.QueueCaused), and a writer queues that statement before it
// commits; a tuple is appended with its cause, an event's id or the sentinel 0, in its first
// statement. A negative source_event that committed is a write that wrote uncaused and was never
// caused, which a read of the store would take for an event. after names what ran, for the
// failure. It reads through the pool, so it sees only what committed.
func RequireNoMarkedCause(t testing.TB, database *store.Store, after string) {
	t.Helper()
	rows, err := database.Pool.Query(context.Background(), `
		(select 'graph_nodes ' || kind || ':' || id || ' at ' || source_event
		 from graph_nodes where source_event < 0 order by kind, id limit 10)
		union all
		(select 'graph_tuples ' || from_kind || ':' || from_id || ' ' || kind || ' at ' || source_event
		 from graph_tuples where source_event < 0 order by seq limit 10)
	`)
	if err != nil {
		t.Fatalf("read the store's negative causes after %s: %v", after, err)
	}
	defer rows.Close()
	var marked []string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatalf("read a negative cause after %s: %v", after, err)
		}
		marked = append(marked, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the store's negative causes after %s: %v", after, err)
	}
	if len(marked) > 0 {
		t.Error(fmt.Sprintf("after %s the store holds committed rows whose source_event is negative, a node write's mark "+
			"that no statement moved onto its cause before the commit: %s", after, strings.Join(marked, "; ")))
	}
}
