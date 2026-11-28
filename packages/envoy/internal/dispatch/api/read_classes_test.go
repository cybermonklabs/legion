package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestEveryGETRouteStatesOneReadClass holds the declarations to classing their reads: every GET
// route the server mounts with its test hooks in (readOnlyTable), read off the declarations rather
// than listed, so a GET route added later fails here until its file
// states a class. It fails for each GET route stating none, each class that is no class at all,
// each readWritesState route with no WriteNote, and each route that is not a GET stating a class.
// It reads them from readClassRefusals, the check register refuses a table by, so the test and the
// refusal cannot drift, and fails once, naming each of them, so one run lists all that is missing.
func TestEveryGETRouteStatesOneReadClass(t *testing.T) {
	t.Parallel()
	table := readOnlyTable()
	gets := 0
	classes := map[readWrites]int{}
	for _, route := range table {
		if route.Method != http.MethodGet {
			continue
		}
		gets++
		classes[route.Read.Writes]++
	}
	if gets == 0 {
		t.Fatal("the table with its test hooks in holds no GET route, so the walk read nothing")
	}
	problems := readClassRefusals(table)
	t.Logf("walked %d GET routes of the table (test hooks in): %d reads, %d GETs that write",
		gets, classes[readWritesNothing], classes[readWritesState])
	if len(problems) > 0 {
		t.Errorf("the read classes have %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}
