package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going/eval"
)

// TestCountCallsFoldsCallToolAndCountsReferenceReads: a try's tool steps count each tool by name,
// a call_tool call under the tool it names and one that names none under call_tool; a read of a
// block's reference, by dispatch:// or by a dashboard URL under the try's server, and a call_tool
// of dispatch_read_issue_references or of dispatch_read_document_references each read the
// reference graph, and a read of an issue, a search, a plan's write and a step that is no call do
// not. The try's --answers-out line carries both.
func TestCountCallsFoldsCallToolAndCountsReferenceReads(t *testing.T) {
	const serverURL = "http://127.0.0.1:4100"
	step := func(tool string, arguments map[string]any) []byte {
		body, err := json.Marshal(map[string]any{
			"call":   map[string]any{"id": "call_1", "tool": tool, "arguments": arguments},
			"result": "a result",
			"error":  false,
		})
		if err != nil {
			t.Fatal(err)
		}
		return body
	}
	callTool := func(tool string, arguments map[string]any) []byte {
		return step("call_tool", map[string]any{"tool": tool, "arguments": arguments})
	}
	steps := [][]byte{
		step("read", map[string]any{"ref": "dispatch://DEMO-1/spec#b-x"}),
		step("read", map[string]any{"ref": serverURL + "/issues/DEMO-1/spec#b-x"}),
		callTool("dispatch_read_issue_references", map[string]any{"issue": "DEMO-1"}),
		callTool("dispatch_read_document_references", map[string]any{"document": "0190c7d2-1111-7222-8333-444444444444"}),
		step("read", map[string]any{"ref": "dispatch://DEMO-1"}),
		step("read", map[string]any{"issue": "DEMO-1"}),
		step("search", map[string]any{"query": "quernstone"}),
		callTool("dispatch_search", map[string]any{"query": "quernstone"}),
		step("find_tools", map[string]any{"need": "what an issue references"}),
		step("call_tool", map[string]any{"arguments": map[string]any{}}),
		step("dispatch_comment", map[string]any{"issue": "DEMO-1", "body": "Done."}),
		[]byte(`{"result": "a step that names no call"}`),
	}

	tools, reads := countCalls(steps, serverURL)
	want := map[string]int{
		"read": 4, "search": 1, "find_tools": 1, "call_tool": 1, "dispatch_comment": 1, "dispatch_search": 1,
		"dispatch_read_issue_references": 1, "dispatch_read_document_references": 1,
	}
	if !maps.Equal(tools, want) {
		t.Errorf("tools = %v, want %v", tools, want)
	}
	if reads != 4 {
		t.Errorf("reference reads = %d, want 4: two reads of a block and the two references tools", reads)
	}

	line, err := json.Marshal(tryResult{Task: "find-references", Tools: tools, ReferenceReads: reads})
	if err != nil {
		t.Fatal(err)
	}
	for _, held := range []string{`"tools":{`, `"dispatch_read_issue_references":1`, `"reference_reads":4`} {
		if !bytes.Contains(line, []byte(held)) {
			t.Errorf("the --answers-out line %s holds no %s", line, held)
		}
	}
}

// TestTheReportListsTheTasksThatReadTheReferenceGraph: the report's section names each task any of
// whose tries read the reference graph, with how many of its tries did, and the --tasks that runs
// them again; a task none of whose tries read it is not named.
func TestTheReportListsTheTasksThatReadTheReferenceGraph(t *testing.T) {
	tasks := []eval.Task{{ID: "trace-links"}, {ID: "close-issue"}, {ID: "find-backlinks"}}
	byTask := map[string][]tryResult{
		"trace-links":    {{Try: 1, ReferenceReads: 2}, {Try: 2}, {Try: 3, ReferenceReads: 1}},
		"close-issue":    {{Try: 1}, {Try: 2}, {Try: 3}},
		"find-backlinks": {{Try: 1, ReferenceReads: 1}, {Try: 2, ReferenceReads: 3}, {Try: 3, ReferenceReads: 1}},
	}
	var report strings.Builder
	writeReferenceReads(&report, tasks, byTask)
	text := report.String()
	for _, held := range []string{
		"## Reads of the reference graph",
		"GET /api/v1/references",
		"which a change to those routes reaches: 2.",
		"- trace-links: 2 of 3 tries\n",
		"- find-backlinks: 3 of 3 tries\n",
		"Run them again with `--tasks trace-links,find-backlinks`.",
	} {
		if !strings.Contains(text, held) {
			t.Errorf("the section holds no %q:\n%s", held, text)
		}
	}
	if strings.Contains(text, "close-issue") {
		t.Errorf("the section names close-issue, none of whose tries read the graph:\n%s", text)
	}
}

// TestATryTheJudgeReadsAgainCountsAndOneStillUnreadLeavesTheRunIncomplete: of two tries the judge
// could not read during the run, one it reads at the run's end is graded by that reading and
// counts as a pass, and one it still cannot read stays unjudged with the second error. The report
// counts the unjudged try apart, by task, with the judge's error, gives no floor over its kind,
// says in its RESULT that the run is incomplete, and does not meet the floors.
func TestATryTheJudgeReadsAgainCountsAndOneStillUnreadLeavesTheRunIncomplete(t *testing.T) {
	tasks := []eval.Task{
		{ID: "answer-01", Kind: eval.KindAnswer, Words: "summarise", Grade: eval.Grade{Facts: []string{"A fact."}}},
		{ID: "answer-02", Kind: eval.KindAnswer, Words: "summarise", Grade: eval.Grade{Facts: []string{"A fact."}}},
	}
	timedOut := "the judge's call ended time: the run went past 3m0s"
	results := []tryResult{
		{Task: "answer-01", Kind: eval.KindAnswer, Try: 1, Unjudged: timedOut, judged: "read on the second pass"},
		{Task: "answer-02", Kind: eval.KindAnswer, Try: 1, Unjudged: timedOut, judged: "never read"},
	}
	read := func(_ context.Context, words, answer string, facts []string) (judgement, error) {
		if answer == "never read" {
			return judgement{}, errors.New("the judge answered no JSON object")
		}
		return judgement{Stated: []bool{true}, Why: []string{"it says it"}}, nil
	}

	if again := judgeAgain(context.Background(), read, tasks, results); len(again) != 2 {
		t.Fatalf("judgeAgain read %v again; want both tries", again)
	}
	if passed := results[0]; !passed.Pass || passed.Unjudged != "" || passed.verdict() != "pass" {
		t.Errorf("the try read on the second pass is %+v; want a pass, judged", passed)
	}
	if unread := results[1]; unread.Pass || unread.Unjudged != "the judge answered no JSON object" || unread.verdict() != "unjudged" {
		t.Errorf("the try still unread is %+v; want it unjudged with the second error", unread)
	}

	var report strings.Builder
	met := writeReport(&report, reportInput{
		options: runOptions{tries: 1, parallel: 1}, all: tasks, tasks: tasks, results: results,
		calibration: calibrationResult{answers: eval.JudgeCalibrationAnswers, agreed: eval.JudgeCalibrationAnswers},
	})
	text := report.String()
	if met {
		t.Errorf("the report with an unjudged try meets every floor:\n%s", text)
	}
	for _, held := range []string{
		"| answer-01 |  |  | pass |  |",
		"| answer-02 |  |  | unjudged | try 1: the judge could not read it: the judge answered no JSON object",
		"| answer | 2 | 100.0% (1 of 1) | 75% not given: 1 unjudged |",
		"## Unjudged tries",
		"- answer-02 try 1: the judge answered no JSON object\n",
		"RESULT: the run is incomplete: the judge could not read 1 of its tries",
	} {
		if !strings.Contains(text, held) {
			t.Errorf("the report holds no %q:\n%s", held, text)
		}
	}
	if strings.Contains(text, "pass^1 of answer") {
		t.Errorf("the report gives a floor verdict over the kind with an unjudged try:\n%s", text)
	}
}

// TestAFactOnlyAStepHeaderStatesFailsByTheAnswerFramesAndPassesOnlyByTheOldRule: the judge reads as
// a try's answer the text of its answer frames alone. The person reads a run whose fact stands
// only in a step header - a reply frame that names tools, which the bar draws over its calls and
// never as the answer - and the try fails its task: the judge is put the answer frame's text, and
// the header's is kept apart as the try's header_text. Grading puts nothing else to the judge; the
// second pass, once every try has ended, grades the try by the old rule, every reply's text joined
// in order, by which it passes, and that grade is the try's old_rule and nothing more: the try
// still fails, the floors count it failed, and the report names it as a try whose verdict
// differs, saying how many tries' headers held text.
func TestAFactOnlyAStepHeaderStatesFailsByTheAnswerFramesAndPassesOnlyByTheOldRule(t *testing.T) {
	const header, answer = "Reading the roster: session-impl-04 holds DEMO-26 and is gone.", "Five agent sessions are working now."
	events := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for id, frame := range []struct{ name, data string }{
			{"reply", `{"step":1,"text":` + strconv.Quote(header) + `,"tools":["call_tool"],"output_tokens":90}`},
			{"tool", `{"step":2,"tool":"call_tool"}`},
			{"reply", `{"step":3,"text":` + strconv.Quote(answer) + `,"output_tokens":40}`},
			{"end", `{"state":"done","reason":"","offers":[]}`},
		} {
			_, _ = fmt.Fprintf(w, "id: %d\nevent: %s\ndata: %s\n\n", id+1, frame.name, frame.data)
		}
	}))
	t.Cleanup(events.Close)
	p := &person{origin: events.URL, login: evalPerson, client: events.Client()}
	result := tryResult{Task: "status-11", Kind: eval.KindStatus, Try: 1}
	if err := p.readFrames(context.Background(), "run-1", &result.Outcome); err != nil {
		t.Fatal(err)
	}
	if result.Outcome.Answer != answer || result.Outcome.HeaderText != header {
		t.Fatalf("the outcome's answer is %q and its header text %q; want the answer frame's text and the header's apart", result.Outcome.Answer, result.Outcome.HeaderText)
	}

	task := eval.Task{ID: "status-11", Kind: eval.KindStatus, Words: "which agents are working now and on what",
		Grade: eval.Grade{Facts: []string{"session-impl-04, which holds DEMO-26, is gone."}}}
	var put []string
	read := func(_ context.Context, _, text string, _ []string) (judgement, error) {
		put = append(put, text)
		stated := strings.Contains(text, "session-impl-04")
		return judgement{Stated: []bool{stated}, Why: []string{fmt.Sprintf("it names the gone session: %t", stated)}}, nil
	}
	gradeAnswer(context.Background(), read, task, &result, &eval.Store{})
	result.Pass = result.passes(task)
	if result.Pass || len(put) != 1 || put[0] != answer || result.OldRule != nil {
		t.Fatalf("graded by the answer frames, the try passes %t, the judge was put %q and its old rule is %+v; want a fail, the answer frame's text alone, and no old rule yet",
			result.Pass, put, result.OldRule)
	}

	results := []tryResult{result}
	if graded := gradeOldRule(context.Background(), read, []eval.Task{task}, results); len(graded) != 1 {
		t.Fatalf("the second pass graded %v; want the one try whose header held text", graded)
	}
	if got := results[0]; got.Pass || got.OldRule == nil || !got.OldRule.Pass || len(put) != 2 || !strings.Contains(put[1], header) || !strings.Contains(put[1], answer) {
		t.Fatalf("after the second pass the try passes %t with the old rule %+v, the judge put %q; want a fail still, an old rule that passes, and every reply's text put to it",
			got.Pass, got.OldRule, put)
	}
	line, err := json.Marshal(results[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, held := range []string{`"header_text":` + strconv.Quote(header), `"old_rule":{"pass":true}`, `"pass":false`} {
		if !strings.Contains(string(line), held) {
			t.Errorf("the --answers-out line %s holds no %s", line, held)
		}
	}

	var report strings.Builder
	writeReport(&report, reportInput{
		options: runOptions{tries: 1, parallel: 1}, all: []eval.Task{task}, tasks: []eval.Task{task}, results: results,
		calibration: calibrationResult{answers: eval.JudgeCalibrationAnswers, agreed: eval.JudgeCalibrationAnswers},
	})
	text := report.String()
	for _, held := range []string{
		"| status-11 |  |  | fail | try 1: fact 1 not stated (it names the gone session: false) | pass |",
		"| status | 1 | 0.0% (0 of 1) |",
		"Tries whose step headers held text: 1 of 1.",
		"Tries whose verdict differs between the two rules: 1.\n- status-11 try 1 (status): by the answer frames fail (fact 1 not stated (it names the gone session: false)); by the old rule pass\n",
	} {
		if !strings.Contains(text, held) {
			t.Errorf("the report holds no %q:\n%s", held, text)
		}
	}
}

// TestEachServerPacesItsShareOfTheTokenLimit: the servers that run at once share the proxy's limit,
// each pacing its calls to the limit over --parallel, at least 1, and the report's header says so.
func TestEachServerPacesItsShareOfTheTokenLimit(t *testing.T) {
	if got := serverTokenLimit(1); got != tokenLimit {
		t.Errorf("one server paces its calls to %d; want the whole %d", got, tokenLimit)
	}
	if got := serverTokenLimit(4); got != tokenLimit/4 {
		t.Errorf("each of four servers paces its calls to %d; want %d", got, tokenLimit/4)
	}
	for parallel := 1; parallel <= 7; parallel++ {
		if sum := parallel * serverTokenLimit(parallel); sum > tokenLimit {
			t.Errorf("%d servers' shares add up to %d, over the proxy's %d", parallel, sum, tokenLimit)
		}
	}
	if got := serverTokenLimit(2 * tokenLimit); got != 1 {
		t.Errorf("a server among more than the limit's tokens paces its calls to %d; want 1", got)
	}
	var report strings.Builder
	writeReport(&report, reportInput{options: runOptions{tries: 1, parallel: 4}})
	if want := "through a limiter of 500000 tokens a minute; each of the 4 servers that run at once paces its own calls to 125000 of them."; !strings.Contains(report.String(), want) {
		t.Errorf("the report's header holds no %q:\n%s", want, report.String())
	}
}
