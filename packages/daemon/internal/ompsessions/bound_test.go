package ompsessions

import (
	"context"
	"strings"
	"testing"
)

// A transaction runs again at most 3 times, so it uses at most 4 server connections, as the
// store's own bound is: on a login that every try refuses (42501, no rights on the tables),
// Written opens its transaction on exactly 4 distinct backends, 3 of them opened in place of one
// it closed, closes each, and names the 4 tries and the last SQLSTATE. The numbers are written out
// here and not read from tries, so a change of the bound fails this test.
func TestATransactionUsesAtMostFourServerConnections(t *testing.T) {
	ctx := context.Background()
	dsn, admin := repairDatabase(t, "legion_ompsessions_bound")
	_, roleURL := testLogin(t, admin, dsn, "legion_ompsessions_bound_norights")
	trace := &tracer{}
	conn := tracedConn(t, roleURL, trace)
	trace.take()
	_, _, err := Written(ctx, conn, podSessionFile)
	sent, connects := trace.take()
	pids := openings(sent)
	if err == nil || !strings.Contains(err.Error(), "each of its 4 tries") || !strings.Contains(err.Error(), "SQLSTATE 42501") {
		t.Errorf("Written as a login every try refuses = %v; want it to fail naming its 4 tries and SQLSTATE 42501", err)
	}
	if connects != 3 || len(pids) != 4 || !distinct(pids) {
		t.Fatalf("the client opened %d connections and opened its transaction on %v; want 3 opened and 4 distinct backends", connects, pids)
	}
	gone(t, admin, pids)
	if !conn.IsClosed() {
		t.Error("after the fourth try the Conn is open, want its connection closed")
	}
	if failure := noRollback(sent); failure != "" {
		t.Error(failure)
	}
}
