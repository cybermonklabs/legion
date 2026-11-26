package agent

//ci:reads packages/contracts/fixtures/shown-values.json os.ReadFile (../../../contracts/fixtures/shown-values.json): TestTheModelIsGivenEachValueInTheWebsWords holds the agent's shown values to the words the web writes

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/going"
	"github.com/sjawhar/envoy/going/agent/agenttest"
	"github.com/sjawhar/envoy/going/agentwire"
)

// shownRow is one row of packages/contracts/fixtures/shown-values.json: a value of a kind, a
// priority, a status or a time, and the words the web writes for it at now.
type shownRow struct {
	Kind  string          `json:"kind"`
	Now   time.Time       `json:"now"`
	Value json.RawMessage `json:"value"`
	Shown string          `json:"shown"`
}

// TestTheModelIsGivenEachValueInTheWebsWords: packages/contracts/fixtures/shown-values.json holds
// the words the web writes for each priority, 0 to 3 and null, and for a time relative to now
// across each threshold of its relativeTimeLabel; the web's Timestamp.test.tsx holds the web to
// it. The agent gives its model the same words beside each such value of a result, in the field
// after it named for it with _shown, so the answer the model writes from them and the page say
// each value alike: a priority of 1, 0 and null reads P1, P0 and "No priority", and a time 13
// days before now reads as the web writes it, "2 weeks ago".
func TestTheModelIsGivenEachValueInTheWebsWords(t *testing.T) {
	raw, err := os.ReadFile("../../../contracts/fixtures/shown-values.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Rows []shownRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]int{}
	for _, row := range fixture.Rows {
		kinds[row.Kind]++
		// The agent gives no words beside a status: the web's board-model.test.ts holds the
		// fixture's status rows to the page's words, and going/eval reads them.
		if row.Kind == "status" {
			continue
		}
		field := map[string]string{"priority": "priority", "time": "closed_at"}[row.Kind]
		if field == "" {
			t.Fatalf("a row of the fixture is of the kind %q, neither priority, status nor time", row.Kind)
		}
		body := []byte(`{"key":"A-1","` + field + `":` + string(row.Value) + `,"title":"A title"}`)
		members := shownMembers(body, row.Now)
		want := "," + jsonString(field+shownSuffix) + ":" + jsonString(row.Shown)
		if len(members) != 1 || members[0].text != want || string(body[members[0].at-len(row.Value):members[0].at]) != string(row.Value) {
			t.Errorf("the %s %s at %s is followed by %+v, want %s after it, as the web writes it", row.Kind, row.Value, row.Now.Format(time.RFC3339), members, want)
		}
	}
	if kinds["priority"] != 5 || kinds["time"] == 0 {
		t.Errorf("the fixture holds %d priorities and %d times, want each of 0 to 3 and null, and times", kinds["priority"], kinds["time"])
	}
}

// TestOnlyAPriorityOrATimeOfAFieldTakesWords: the words follow a field of an object at any depth,
// in a list's objects too, and only a priority that holds 0 to 3 or null and a field that holds an
// RFC 3339 time: a priority past 3, written as text or with a fraction, a date with no time, a time
// in a list, which names no field, and a null time take none. A body cut short gives the words of
// the values before its cut, and a body that is no JSON none.
func TestOnlyAPriorityOrATimeOfAFieldTakesWords(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	words := func(body string) string {
		var out []string
		for _, member := range shownMembers([]byte(body), now) {
			out = append(out, member.text)
		}
		return strings.Join(out, "")
	}
	for body, want := range map[string]string{
		`{"issue":{"priority":2,"claim":{"since":"2026-11-18T11:00:00Z"}},"children":[{"priority":3},{"priority":null}]}`: `,"priority_shown":"P2","since_shown":"1 hour ago","priority_shown":"P3","priority_shown":"No priority"`,
		`{"priority":4,"other":{"priority":"1"},"rank":{"priority":1.0},"due":"2026-11-20","closed_at":null}`:             ``,
		`{"labels":["2026-11-18T11:00:00Z"],"ok":true,"count":2}`:                                                         ``,
		`[{"updated_at":"2026-11-17T12:00:00Z"},{"updated_at":"2026-11-1`:                                                 `,"updated_at_shown":"yesterday"`,
		`The issue is P1 and closed 2026-11-17T12:00:00Z.`:                                                                ``,
	} {
		if got := words(body); got != want {
			t.Errorf("%s takes the words %q, want %q", body, got, want)
		}
	}
}

// TestTheWordsStayWithinTheBoundOfAResult: a result the model is given holds at most
// modelResultLimit bytes of its body, its words counted. A body the model was given whole stays
// whole: where the words of all its values would pass the bound, its priorities take theirs
// first, then its times, in the body's order, as many as fit. The seed's list of DEMO's issues is
// such a body: 31,349 bytes, whose 142 values would take some 4 KiB of words. A body resultText
// already cut at the bound is cut further, so each value it gives but the last takes its words,
// and its note names the bytes given and the whole answer's size. A result that holds no call's
// answer, such as a refusal, is given as it was saved.
func TestTheWordsStayWithinTheBoundOfAResult(t *testing.T) {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	call := agentwire.CallRequest{Step: 4, Call: 1}
	issue := `{"key":"A-1","priority":1,"updated_at":"2026-11-18T11:00:00Z"}`
	bodyOf := func(issues int) []byte {
		return []byte("[" + strings.TrimSuffix(strings.Repeat(issue+",", issues), ",") + "]")
	}
	// given is the body the model is given of result, the raw bytes of it the note names, and the
	// size of the whole answer the note names, 0 where it has no note.
	given := func(t *testing.T, result string) (body string, raw int, total int) {
		t.Helper()
		_, rest, _ := strings.Cut(strings.TrimPrefix(result, storedDataNote+"\n"), "\n")
		// The list's total stands on a line of its own before the body (resultCount).
		if line, after, lined := strings.Cut(rest, "\n"); lined && resultTotalLine.MatchString(line) {
			rest = after
		}
		body, note, shortened := strings.Cut(rest, shortenedPrefix)
		if len(body) > modelResultLimit {
			t.Fatalf("the model is given %d bytes of the body, past the bound of %d", len(body), modelResultLimit)
		}
		if !shortened {
			return body, len(body), 0
		}
		if _, err := fmt.Sscanf(note, "%d bytes and you were given the first %d:", &total, &raw); err != nil {
			t.Fatalf("the note after the body reads %q: %v", note, err)
		}
		return body, raw, total
	}
	words := regexp.MustCompile(`,"[a-z_]+_shown":"[^"]*"`)

	small := bodyOf(2)
	result := resultText(call, agentwire.CallAnswer{Status: http.StatusOK, Body: small})
	body, _, total := given(t, shownResult(result, now))
	if want := strings.ReplaceAll(string(small), `:1,`, `:1,"priority_shown":"P1",`); total != 0 || body != strings.ReplaceAll(want, `11:00:00Z"`, `11:00:00Z","updated_at_shown":"1 hour ago"`) {
		t.Errorf("a small result is given as %q, shortened to %d, want each value followed by its words and no note", body, total)
	}

	// A whole body under the bound, with room for the words of its priorities and a few of its
	// times.
	whole := bodyOf(modelResultLimit / (len(issue) + 1 + 27))
	result = resultText(call, agentwire.CallAnswer{Status: http.StatusOK, Body: whole})
	body, _, total = given(t, shownResult(result, now))
	issues, priorities, times := strings.Count(string(whole), `"priority":1`), strings.Count(body, `"priority_shown":"P1"`), strings.Count(body, `"updated_at_shown"`)
	if stripped := words.ReplaceAllString(body, ""); total != 0 || stripped != string(whole) || priorities != issues || times == 0 || times >= issues ||
		!strings.HasPrefix(body, "["+strings.Repeat(`{"key":"A-1","priority":1,"priority_shown":"P1","updated_at":"2026-11-18T11:00:00Z","updated_at_shown":"1 hour ago"},`, times)) {
		t.Errorf("a whole body of %d issues is given as %d bytes, shortened to %d, with %d priorities and %d times in words, want it whole, every priority and the first times in words",
			issues, len(body), total, priorities, times)
	}

	// A body resultText already cut at the bound is given as its first bytes, each value but
	// the last followed by its words, and the note names the size of the whole answer.
	long := bodyOf(2 * modelResultLimit / len(issue))
	result = resultText(call, agentwire.CallAnswer{Status: http.StatusOK, Body: long})
	if !strings.Contains(result, shortenedPrefix) {
		t.Fatalf("a body of %d bytes was saved whole", len(long))
	}
	body, raw, total := given(t, shownResult(result, now))
	stripped := words.ReplaceAllString(body, "")
	if total != len(long) || raw != len(stripped) || stripped != string(long[:raw]) || len(body) < modelResultLimit-len(issue)-80 {
		t.Errorf("a body of %d bytes is given as %d bytes, %d of them the answer's, with a note naming %d of %d, want its first bytes near the bound of %d and a note naming them and %d",
			len(long), len(body), len(stripped), raw, total, modelResultLimit, len(long))
	}
	if priorities, shown := strings.Count(stripped, `"priority":1`), strings.Count(body, `"priority_shown":"P1"`); shown < priorities-1 || shown == 0 {
		t.Errorf("a cut body is given with %d of its %d priorities in words, want each but the last", shown, priorities)
	}

	refusal := "the call was refused (BAD_REQUEST): no such project"
	if got := shownResult(refusal, now); got != refusal {
		t.Errorf("a refusal is given as %q, want it as saved", got)
	}
}

// TestTheModelReadsEachPriorityAndTimeAsThePageShowsIt: a run whose agent is given a time now
// (Config.Now, DISPATCH_TEST_AGENT_NOW under a test's server) tells its model that time in the
// person's message, and gives it each priority and time of a result in the page's words beside
// the value, the times relative to that now: priority 1, 0 and null read P1, P0 and "No
// priority", a closed_at 13 days before now "2 weeks ago". Each time's words follow the time
// itself, as the result holds it, which the answer writes as its time link's address. The step
// saves the result as the server answered it, which ReadSavedResult reads, and a draw_block over
// that list counts its raw values.
func TestTheModelReadsEachPriorityAndTimeAsThePageShowsIt(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	const list = `[{"key":"A-1","priority":1,"closed_at":"2026-11-10T12:00:00Z"},{"key":"A-2","priority":0,"closed_at":null},` +
		`{"key":"A-3","priority":null,"updated_at":"2026-11-23T11:55:00Z"}]`
	fake := agenttest.New(t, func(request *agenttest.Request) agenttest.Reply {
		if len(request.ToolResults()) == 0 {
			return agenttest.Reply{Calls: []agenttest.Call{{Name: toolSearch, Arguments: map[string]any{"query": "closed issues"}}}}
		}
		return agenttest.Reply{Text: "A-1 is P1 and closed 2 weeks ago."}
	})
	server, client := newFakeServer(t, func(agentwire.CallRequest) agentwire.CallAnswer {
		return agentwire.CallAnswer{Status: http.StatusOK, Body: []byte(list)}
	})
	a := &agent{client: client, config: Config{Model: fake.Model(), Now: now}, held: map[string]*worker{}, claimed: make(chan struct{}, 1), exit: make(chan struct{})}
	run := &agentwire.ClaimedRun{ID: "run-1", Words: "which issues closed", Level: "fast", Mode: "ask", LeaseToken: "lease-1"}
	newWorker(t.Context(), a, run, func() {}).work()
	if end, _ := server.end(t); end.State != agentwire.StateDone {
		t.Fatalf("the run ended %s with %q, want done", end.State, end.Reason)
	}
	requests := stepRequests(fake)
	if len(requests) != 2 {
		t.Fatalf("the run made %d model calls, want 2: the search and the answer", len(requests))
	}
	for index, request := range requests {
		if text := request.Text(); !strings.Contains(text, "The time now: 2026-11-23 12:00 UTC.") {
			t.Errorf("model call %d is told %q, which does not state the agent's time now", index+1, text)
		}
	}
	told := strings.Join(requests[1].ToolResults(), "\n")
	for _, want := range []string{
		`"priority":1,"priority_shown":"P1"`,
		`"priority":0,"priority_shown":"P0"`,
		`"priority":null,"priority_shown":"No priority"`,
		`"closed_at":"2026-11-10T12:00:00Z","closed_at_shown":"2 weeks ago"`,
		`"updated_at":"2026-11-23T11:55:00Z","updated_at_shown":"5 minutes ago"`,
	} {
		if !strings.Contains(told, want) {
			t.Errorf("the model is given the result %q, which does not hold %s", told, want)
		}
	}
	if strings.Contains(told, `"closed_at":null,`+`"closed_at_shown"`) {
		t.Errorf("the model is given the result %q, which writes words for a null time", told)
	}

	server.mu.Lock()
	var saved []agentwire.StepRequest
	for _, step := range server.steps {
		if step.Kind == agentwire.StepTool {
			saved = append(saved, step)
		}
	}
	server.mu.Unlock()
	if len(saved) != 1 {
		t.Fatalf("the run saved %d tool steps, want the search's", len(saved))
	}
	result, err := ReadSavedResult(saved[0].Body)
	if err != nil || string(result.Body) != list || strings.Contains(string(saved[0].Body), shownSuffix) {
		t.Fatalf("the saved step holds %s (%v), want the search's answer as the server gave it, with no words beside it", saved[0].Body, err)
	}
	block, err := going.ParseRunBlock(json.RawMessage(fmt.Sprintf(`{"kind":"table","title":"Issues by priority","steps":[{"step":%d,"call":%d}],`+
		`"chain":[{"op":"group","field":"priority"},{"op":"count","as":"issues"},{"op":"sort","field":"priority"}],"columns":["priority","issues"]}`, saved[0].Step, result.Call)))
	if err != nil {
		t.Fatal(err)
	}
	drawn, err := block.Draw([]going.Result{{Step: saved[0].Step, Call: result.Call, Tool: result.Tool, Arguments: result.Arguments, Status: result.Status, Body: result.Body}})
	if err != nil {
		t.Fatal(err)
	}
	rows, _ := json.Marshal(drawn.Rows)
	if want := `[{"issues":1,"priority":0},{"issues":1,"priority":1},{"issues":1,"priority":"none"}]`; string(rows) != want {
		t.Errorf("a block over the saved list counts %s, want the raw priorities counted, %s", rows, want)
	}
}
