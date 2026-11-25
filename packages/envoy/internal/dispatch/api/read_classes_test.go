package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestEveryGETRowHasOneReadEntry holds readClasses to the table it classes: every GET row of the
// table with its test hooks in (readOnlyTable, the table routesNotMounted walks too), read off
// the table rather than listed, so a GET row added later fails here until it has an entry. It
// fails for each GET row with no entry, each entry whose Writes is unset or no class at all, each
// readWritesState entry with no WriteNote, and each entry that names no GET row the table
// mounts. It reads them from readClassRefusals, the check register refuses a table by, so the
// test and the refusal cannot drift, and fails once, naming each of them, so one run lists all
// that is missing.
func TestEveryGETRowHasOneReadEntry(t *testing.T) {
	t.Parallel()
	gets := 0
	classes := map[readWrites]int{}
	for _, route := range readOnlyTable() {
		if route.Method != http.MethodGet {
			continue
		}
		gets++
		classes[readClasses[route.Method+" "+route.Pattern].Writes]++
	}
	if gets == 0 {
		t.Fatal("the table with its test hooks in holds no GET row, so the walk read nothing")
	}
	problems := readClassRefusals()
	t.Logf("walked %d GET rows of the table (test hooks in): %d reads, %d GETs that write; readClasses holds %d entries",
		gets, classes[readWritesNothing], classes[readWritesState], len(readClasses))
	if len(problems) > 0 {
		t.Errorf("the read class (read_classes.go) has %d problems:\n%s", len(problems), strings.Join(problems, "\n"))
	}
}
