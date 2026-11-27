package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"
)

// policyRow is one row of a hand-made table: name is "<method> <pattern>", and next is the
// /api/v2 route an /api/v1 row names as its successor. A /api/v2 row is the mount of a routeRow,
// which the decoder reads (apiRoute.decoded), as every /api/v2 row of the table is but the
// INFRA-979 tree's.
func policyRow(name string, version apiVersion, next successor, body requestBody) apiRoute {
	method, pattern, _ := strings.Cut(name, " ")
	return apiRoute{routeRow: routeRow{Method: method, Pattern: pattern, Body: body}, Version: version, Successor: next, columns: version == apiV2}
}

// policyTable is a table the approve list is compiled against in the tests below: /api/v1 routes
// with each kind of successor the list may meet, a successor taken whole, one taken by state, one
// taken by state that the decoder does not read (a row of the INFRA-979 tree), one that is
// deleted, one that is unmounted and one that is none, and the /api/v2 routes that answer for them.
func policyTable() []apiRoute {
	states := requestBody{Kind: bodyStateUnion, Arms: map[string]any{"approved": struct{}{}, "denied": struct{}{}, "dismissed": struct{}{}}}
	undecoded := policyRow("PUT /api/v2/old/{id}/state", apiV2, successor{}, states)
	undecoded.OfINFRA979 = true
	return []apiRoute{
		policyRow("POST /api/v1/things/{id}/review", apiV1, successorIs(http.MethodPost, "/things/{id}/reviews"), requestBody{}),
		policyRow("POST /api/v1/things/{id}/approve", apiV1, successorIs(http.MethodPut, "/things/{id}/state"), requestBody{}),
		policyRow("POST /api/v1/things/{id}/deny", apiV1, successorIs(http.MethodPut, "/things/{id}/state"), requestBody{}),
		policyRow("POST /api/v1/things/{id}/dismiss", apiV1, successorIs(http.MethodPut, "/things/{id}/state"), requestBody{}),
		policyRow("DELETE /api/v1/plain", apiV1, successorIs(http.MethodDelete, "/plain"), requestBody{}),
		policyRow("POST /api/v1/old/{id}/approve", apiV1, successorIs(http.MethodPut, "/old/{id}/state"), requestBody{}),
		policyRow("POST /api/v1/gone", apiV1, deletedFor(http.MethodPost, "/things/{id}/reviews"), requestBody{}),
		policyRow("POST /api/v1/elsewhere", apiV1, successorIs(http.MethodPost, "/missing"), requestBody{}),
		policyRow("POST /api/v1/unrelated", apiV1, successor{}, requestBody{}),
		policyRow("POST /api/v2/things/{id}/reviews", apiV2, successor{}, requestBody{Kind: bodyJSON}),
		policyRow("PUT /api/v2/things/{id}/state", apiV2, successor{}, states),
		policyRow("DELETE /api/v2/plain", apiV2, successor{}, requestBody{}),
		undecoded,
	}
}

// TestTheApprovePolicyStampsTheRowsTheListNames: compiling an approve list against a table marks
// ApproveOnly on each /api/v1 route the list names and on the /api/v2 route each has as its
// successor, gives a successor taken by state the states its entries name and a successor taken
// whole none, marks the routes of an entry that answers asks of every kind, and leaves every other
// row as it was, including a route that shares a successor with one the list names.
func TestTheApprovePolicyStampsTheRowsTheListNames(t *testing.T) {
	t.Parallel()
	table := policyTable()
	list := []approveRoute{
		{v1: "POST /api/v1/things/{id}/review"},
		{v1: "POST /api/v1/things/{id}/approve", state: "approved"},
		{v1: "POST /api/v1/things/{id}/deny", state: "denied", approvalsOnly: true},
		{v1: "DELETE /api/v1/plain"},
	}
	if faults := applyApprovePolicy(table, list); len(faults) != 0 {
		t.Fatalf("the faults of a list that maps onto the table = %q; want none", faults)
	}
	type stamp struct {
		only   bool
		states []string
		asks   bool
	}
	want := map[string]stamp{
		"POST /api/v1/things/{id}/review":  {only: true},
		"POST /api/v1/things/{id}/approve": {only: true},
		"POST /api/v1/things/{id}/deny":    {only: true, asks: true},
		"DELETE /api/v1/plain":             {only: true},
		"POST /api/v2/things/{id}/reviews": {only: true},
		"PUT /api/v2/things/{id}/state":    {only: true, states: []string{"approved", "denied"}, asks: true},
		"DELETE /api/v2/plain":             {only: true},
	}
	for _, route := range table {
		name := route.Method + " " + route.Pattern
		got, expected := stamp{route.ApproveOnly, route.ApproveStates, route.ApproveApprovalAsks}, want[name]
		if got.only != expected.only || got.asks != expected.asks || !slices.Equal(got.states, expected.states) {
			t.Errorf("%s is stamped %+v; want %+v", name, got, expected)
		}
	}
}

// TestTheApprovePolicyRefusesAListThatDoesNotMapOntoTheTable: each way an approve list can open
// less or more than it names is a fault the compile reports, naming the route it concerns, and a
// table with any fault is left unstamped: a route the table does not mount, one that is no
// /api/v1 route, one listed twice; a route with no successor, a deleted one, or one the table does
// not mount; a state for a successor that takes none or one the decoder does not read, a state it
// does not declare, and no state for a successor that takes several operations by state. Every
// fault is reported, sorted.
func TestTheApprovePolicyRefusesAListThatDoesNotMapOntoTheTable(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		list []approveRoute
		// faults is what each fault begins with, in the order they are reported.
		faults []string
	}{
		{"a route the table does not mount", []approveRoute{{v1: "POST /api/v1/nowhere"}}, []string{"POST /api/v1/nowhere"}},
		{"a route that is no /api/v1 route", []approveRoute{{v1: "POST /api/v2/things/{id}/reviews"}}, []string{"POST /api/v2/things/{id}/reviews"}},
		{"a route listed twice", []approveRoute{{v1: "DELETE /api/v1/plain"}, {v1: "DELETE /api/v1/plain"}}, []string{"DELETE /api/v1/plain"}},
		{"a route with no successor", []approveRoute{{v1: "POST /api/v1/unrelated"}}, []string{"POST /api/v1/unrelated"}},
		{"a route whose successor is deleted", []approveRoute{{v1: "POST /api/v1/gone"}}, []string{"POST /api/v1/gone"}},
		{"a route whose successor the table does not mount", []approveRoute{{v1: "POST /api/v1/elsewhere"}}, []string{"POST /api/v1/elsewhere"}},
		{"a state for a successor that takes none", []approveRoute{{v1: "POST /api/v1/things/{id}/review", state: "approved"}}, []string{"POST /api/v1/things/{id}/review"}},
		{"a state for a successor the decoder does not read", []approveRoute{{v1: "POST /api/v1/old/{id}/approve", state: "approved"}}, []string{"POST /api/v1/old/{id}/approve"}},
		{"a state the successor does not declare", []approveRoute{{v1: "POST /api/v1/things/{id}/approve", state: "settled"}}, []string{"POST /api/v1/things/{id}/approve"}},
		{"no state for a successor that takes several", []approveRoute{{v1: "POST /api/v1/things/{id}/approve"}}, []string{"POST /api/v1/things/{id}/approve"}},
		{
			"every fault, sorted",
			[]approveRoute{{v1: "POST /api/v1/unrelated"}, {v1: "POST /api/v1/nowhere"}, {v1: "DELETE /api/v1/plain"}},
			[]string{"POST /api/v1/nowhere", "POST /api/v1/unrelated"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			table := policyTable()
			faults := applyApprovePolicy(table, test.list)
			if len(faults) != len(test.faults) {
				t.Fatalf("the faults = %q; want one for each of %q", faults, test.faults)
			}
			for index, fault := range faults {
				if !strings.HasPrefix(fault, test.faults[index]) {
					t.Errorf("fault %d = %q; want one naming %s", index, fault, test.faults[index])
				}
			}
			for _, route := range table {
				if route.ApproveOnly || route.ApproveStates != nil || route.ApproveApprovalAsks {
					t.Errorf("%s %s is stamped although the list was refused", route.Method, route.Pattern)
				}
			}
		})
	}
}
