//go:build !race

package api

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// The route window table holds the write routes INFRA-636 changed that had no xid window test
// of their own (xid_window_test.go) to the rule those tests hold theirs to (A1): a route holds
// its transaction id for its database statements alone, with no document work and no
// statement-free gap inside its window. Each row is a route, readied the way the route table
// readies it (route_owner_lock_test.go, routeOwnerLockCase.ready) - the rows it shares with that
// table borrow its ready function, the rest have one of their own beside their row - on a server
// whose pool carries the xid tracer and whose issue's spec is the load harness's large one
// (xidSpec), so a document read or render a route ran inside its window would show as the gap it
// is. The request goes through the fixture's own server under a marked context
// (storetest.MarkXid), so only the transactions the request itself ran are judged, and once the
// route answered the row's own check holds it to having made the write it means, so a window
// measured over a refusal is a rig failure and never a pass.
//
// Every row declares its window (storetest.XidWindowOf): the most statements one transaction of
// the request may run while it holds its transaction id, every one of them named. A route whose
// window holds only a write and its COMMIT passes trivially, and that is what the row is for: it
// shows the window rather than asserting it. A route added to the table is a row added to the
// file of its kind.

// routeXidWindowCase is one row of the window table.
type routeXidWindowCase struct {
	// name is the subtest's name.
	name string
	// route is the route and the function that serves it, as a failure names it.
	route string
	// ready stands the row's server up on base and readies its request (routeOwnerLockCase.ready).
	ready func(t *testing.T, base routeFixture) lockedItemWrite
	// window is what each transaction of the request may hold while it holds its transaction id.
	window storetest.XidShape
	// closedIssue says the request is made on a closed issue, whose documents owe their
	// settlements until it reopens, so the measurement waits for the database to fall quiet
	// rather than for every settlement (awaitQuietDatabase).
	closedIssue bool
}

// routeXidWindowTable is the window table, its rows by kind of route, each kind in a file of its
// own: documentRouteXidWindows (route_xid_window_documents_test.go), issueRouteXidWindows
// (route_xid_window_issues_test.go) and messageRouteXidWindows (route_xid_window_messages_test.go).
var routeXidWindowTable = slices.Concat(documentRouteXidWindows, issueRouteXidWindows, messageRouteXidWindows)

// TestXidWindowRouteTable measures every row of routeXidWindowTable.
func TestXidWindowRouteTable(t *testing.T) {
	// Not parallel: the xid rig measures only in a test that runs alone in the process
	// (XidTracer.Measure fails in a parallel test, or in a subtest of one).
	for _, row := range routeXidWindowTable {
		t.Run(row.name, func(t *testing.T) { measureRouteXidWindow(t, row) })
	}
}

// measureRouteXidWindow readies row on a traced server with the harness's large spec, waits for
// every settlement the seeding owed to finish (for a closed issue, for the database to fall
// quiet), so the request is measured alone, and judges the transactions the request ran against
// the row's window.
func measureRouteXidWindow(t *testing.T, row routeXidWindowCase) {
	t.Helper()
	tracer := storetest.NewXidTracer()
	spec, _ := xidSpec(1)
	write := row.ready(t, routeFixture{options: testServerOptions{tracer: tracer}, spec: spec})
	awaitQuietDatabase(t, write.database, !row.closedIssue)
	marked := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		write.server.ServeHTTP(w, r.WithContext(storetest.MarkXid(r.Context())))
	})
	var answer *httptest.ResponseRecorder
	report := tracer.Measure(t, write.database.Pool, func() { answer = write.send(marked) })
	write.answered(t, answer)
	t.Logf("route %s", row.route)
	storetest.AssertXidWindows(t, report, row.window)
}
