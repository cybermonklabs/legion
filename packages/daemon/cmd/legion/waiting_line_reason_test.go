package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/api"
)

// waitingMember is one issue of the waiting line as the state carries it: its place in the line,
// the part it waits in and the daemon's own rendered sentence for why it waits there
// (api.WaitingLineView). The block under `admission:` reads those three and the issue's key and
// nothing else, so the rest of the issue is left as the zero value.
func waitingMember(key string, place, length int, part, reason string) api.Issue {
	return api.Issue{Key: key, WaitingLine: &api.WaitingLineView{
		Place: place, Length: length, Part: part, Reason: reason}}
}

// An operator reading plain `legion state` is given every waiting issue's place in words, under
// the counts the `admission:` line gives: the header with how many wait, then one line per issue
// with its key, its place, its part and the daemon's own sentence, in the order `waiting`
// publishes them.
//
// The four cases the block prints and no other: nothing waiting; a daemon that served this
// caller no reason at all, which is told from an empty line by `waiting` naming issues while not
// one of them carries a reason — the answer both for a daemon below the contract that added the
// sentence and for one below the contract that added `waitingLine` itself, which sends no such
// key; ten or fewer waiting, named in full with no count after them; and more than ten, where
// ten are named and the last line says how many more there are
// (waitingLineNamedCap, and `legion state --json` for all of them).
func TestWaitingLineReasonIsPrintedUnderTheAdmissionCounts(t *testing.T) {
	returned := "Its work came back at 2026-11-10T15:11:32Z. It is 1st of 1 issue whose work came back, " +
		"and it takes the free slots first."
	queued := "It has not started. It is %d of 2 issues that have not started, by its own Dispatch rank, " +
		"and 1 issue whose work came back stands ahead of all of them."
	three := map[string]api.Issue{
		"LEGION-12": waitingMember("LEGION-12", 1, 3, "returned", returned),
		"LEGION-20": waitingMember("LEGION-20", 2, 3, "starting", fmt.Sprintf(queued, 1)),
		"LEGION-11": waitingMember("LEGION-11", 3, 3, "starting", fmt.Sprintf(queued, 2)),
	}
	// Twelve waiting, which is two more than the block names.
	long, longLine := map[string]api.Issue{}, make([]string, 12)
	for i := range longLine {
		key := fmt.Sprintf("LEGION-%d", 100+i)
		longLine[i] = key
		long[key] = waitingMember(key, i+1, 12, "starting",
			fmt.Sprintf("It has not started. It is %s of 12 issues that have not started, by its own Dispatch rank.", englishPlace(i+1)))
	}
	named := make([]string, 0, 11)
	for _, key := range longLine[:10] {
		named = append(named, "  "+key+" "+fmt.Sprint(long[key].WaitingLine.Place)+" of 12, starting: "+long[key].WaitingLine.Reason)
	}
	named = append(named, "  and 2 more")
	// A member of a daemon at the contract that added `waitingLine` and below the one that added
	// the sentence: a place and no reason. Beside it a member of a daemon below both, carrying no
	// `waitingLine` at all, which is the same case and takes the same line.
	placeOnly := map[string]api.Issue{
		"LEGION-12": waitingMember("LEGION-12", 1, 2, "returned", ""),
		"LEGION-20": {Key: "LEGION-20"},
	}
	for _, tc := range []struct {
		name    string
		waiting []string
		issues  map[string]api.Issue
		want    []string
	}{
		{
			name: "nothing waiting",
			want: []string{"waiting line: none"},
		},
		{
			name:    "no reason at this caller's contract",
			waiting: []string{"LEGION-12", "LEGION-20"},
			issues:  placeOnly,
			want:    []string{"waiting line: the daemon sent no reasons at this caller's contract"},
		},
		{
			name:    "three waiting, named in full",
			waiting: []string{"LEGION-12", "LEGION-20", "LEGION-11"},
			issues:  three,
			want: []string{
				"waiting line: 3",
				"  LEGION-12 1 of 3, returned: " + returned,
				"  LEGION-20 2 of 3, starting: " + fmt.Sprintf(queued, 1),
				"  LEGION-11 3 of 3, starting: " + fmt.Sprintf(queued, 2),
			},
		},
		{
			name:    "twelve waiting, ten named and the rest counted",
			waiting: longLine,
			issues:  long,
			want:    append([]string{"waiting line: 12"}, named...),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := waitingLineLines(tc.waiting, tc.issues); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("waitingLineLines =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}

	// The block is printed where an operator reads it: under the `admission:` counts of plain
	// `legion state`, from the body the daemon serves.
	served := `{"daemon":{"project":"LEGION","schemaVersion":6,"boots":1,"firstBootAt":"2026-09-23T00:00:00Z","startedAt":"2026-09-23T00:00:00Z"},` +
		`"admission":{"cap":2,"active":["LEGION-10"],"waiting":["LEGION-12"]},"pendingStatusWrites":[],` +
		`"issues":{"LEGION-12":{"key":"LEGION-12","generation":1,"phase":"merging","status":"retro",` +
		`"waitingLine":{"place":1,"length":1,"part":"returned","kind":"returning","partPlace":1,"partLength":1,` +
		`"holdsSlot":false,"since":"2026-11-10T15:11:32Z","reason":` + fmt.Sprintf("%q", returned) + `}}}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(served))
	}))
	defer server.Close()
	t.Chdir(t.TempDir())
	t.Setenv("LEGION_DAEMON_URL", server.URL)
	t.Setenv("LEGION_ISSUE", "")

	var out, errb bytes.Buffer
	if code := run(context.Background(), []string{"legion", "state"}, &out, &errb); code != 0 {
		t.Fatalf("legion state = %d, stderr %q", code, errb.String())
	}
	want := "1 waiting; 0 live trees; 1 issues\nwaiting line: 1\n  LEGION-12 1 of 1, returned: " + returned + "\n"
	if !strings.Contains(out.String(), want) {
		t.Fatalf("legion state printed %q, want the block %q under the admission counts", out.String(), want)
	}
}

// englishPlace is the ordinal this test writes its fixture sentences with, which the daemon's own
// render writes with a function of its own (projection's englishOrdinal): the fixture states the
// words rather than reading them from the code that produced them, so a change to either is read
// here as the change it is.
func englishPlace(n int) string {
	return map[int]string{
		1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 5: "5th", 6: "6th",
		7: "7th", 8: "8th", 9: "9th", 10: "10th", 11: "11th", 12: "12th",
	}[n]
}
