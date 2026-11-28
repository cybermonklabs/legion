package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/envoy/going/agent"
	"github.com/sjawhar/envoy/going/eval"
)

// TestTheReportGivesEachLevelsCallTokensAndEveryBudgetStop: the report gives, for each level of the
// tasks run, the p99 and the max of one model call's output tokens and of its reasoning tokens,
// over every model call of every try of the level, beside the level's bound; a level no task ran
// has no row. It names each run stopped at a model call's output tokens, with the tokens of its
// last model call, or that no frame says them, and each run stopped at its working time, with how
// long the pacer held its model calls of the time it worked. The fast tries make 100 calls, whose
// p99 is the 99th of them and whose max the cut call.
func TestTheReportGivesEachLevelsCallTokensAndEveryBudgetStop(t *testing.T) {
	tasks := []eval.Task{
		{ID: "answer-01", Kind: eval.KindAnswer, Level: eval.LevelFast},
		{ID: "status-02", Kind: eval.KindStatus, Level: eval.LevelStandard},
		{ID: "change-03", Kind: eval.KindChange, Level: eval.LevelFast},
	}
	calls := func(from, to int) []modelCall {
		var made []modelCall
		for i := from; i <= to; i++ {
			made = append(made, modelCall{OutputTokens: 10 * i, ReasoningTokens: 3 * i})
		}
		return made
	}
	cut := agent.ReasonOutputTokens + ": a model call of this run wrote the 1000 tokens a call of a fast run may; Continue starts a follow-up with a new budget"
	timed := agent.ReasonTimeBudget + ": this run worked the 5m0s a standard run may, and the model was busy for 3m12s of it; Continue starts a follow-up with a new budget"
	results := []tryResult{
		{Task: "answer-01", Try: 1, Outcome: tryOutcome{State: "done", ModelCalls: calls(1, 60)}},
		{Task: "answer-01", Try: 2, Outcome: tryOutcome{State: "done", ModelCalls: calls(61, 99)}},
		{Task: "status-02", Try: 1, Outcome: tryOutcome{State: "stopped", Reason: timed, WorkingMS: 300_400, PaceMS: 192_300,
			ModelCalls: []modelCall{{OutputTokens: 200, ReasoningTokens: 50}, {OutputTokens: 900, ReasoningTokens: 400}}}},
		{Task: "status-02", Try: 2, Outcome: tryOutcome{State: "done"}},
		{Task: "change-03", Try: 1, Outcome: tryOutcome{State: "stopped", Reason: cut,
			ModelCalls: []modelCall{{OutputTokens: 1310, ReasoningTokens: 310}}}},
		{Task: "change-03", Try: 2, Outcome: tryOutcome{State: "stopped", Reason: cut}},
	}
	var report strings.Builder
	writeReport(&report, reportInput{options: runOptions{tries: 2, parallel: 1}, all: tasks, tasks: tasks, results: results})
	text := report.String()
	fast, standard := agent.Levels["fast"], agent.Levels["standard"]
	for _, held := range []string{
		"## Model calls of each level",
		fmt.Sprintf("| fast | 100 | 990 | 1310 | 297 | 310 | %d | %d |\n", fast.OutputTokens, fast.ReasoningTokens),
		fmt.Sprintf("| standard | 2 | 900 | 900 | 400 | 400 | %d | %d |\n", standard.OutputTokens, standard.ReasoningTokens),
		"Runs stopped at a model call's output tokens (OUTPUT_TOKEN_BUDGET): 2.",
		"- change-03 try 1, fast: its last model call wrote 1310 output tokens, 310 of them reasoning\n",
		"- change-03 try 2, fast: no reply frame says the tokens of its model calls\n",
		"Runs stopped at their working time (TIME_BUDGET): 1,",
		"- status-02 try 1, standard: the pacer held its model calls 3m12.3s of the 5m0.4s it worked\n",
	} {
		if !strings.Contains(text, held) {
			t.Errorf("the report holds no %q:\n%s", held, text)
		}
	}
	if strings.Contains(text, "| deep |") {
		t.Errorf("the report gives a row of deep, which no task ran:\n%s", text)
	}
}

// TestTheReportGivesTheChecksOfEachKindAnswerAndStatusFirst: the report counts the no-plan guard's
// checks of each kind of task, answer and status first, and of all tasks: the turns the tries'
// runs ended, those that made a check, the checks made, the turns that skipped it, those Jev gave
// no line, and the median and the p99 of a turn's checks' time summed, over the turns that made
// one; then how many checks the run made and the tokens they spent. A turn whose reply the check
// held back makes two checks, which the turn's time adds up. A kind none of whose turns made a
// check has no time.
func TestTheReportGivesTheChecksOfEachKindAnswerAndStatusFirst(t *testing.T) {
	tasks := []eval.Task{
		{ID: "find-01", Kind: eval.KindFind, Level: eval.LevelFast},
		{ID: "answer-01", Kind: eval.KindAnswer, Level: eval.LevelFast},
		{ID: "status-01", Kind: eval.KindStatus, Level: eval.LevelFast},
		{ID: "change-01", Kind: eval.KindChange, Level: eval.LevelFast},
	}
	results := []tryResult{
		{Task: "find-01", Try: 1, Outcome: tryOutcome{Turns: 1, Checks: []proseCheck{{Turn: 1, MS: 700, Tokens: 500}}}},
		{Task: "answer-01", Try: 1, Outcome: tryOutcome{Turns: 1, Checks: []proseCheck{{Turn: 1, MS: 400, Tokens: 450}}}},
		{Task: "answer-01", Try: 2, Outcome: tryOutcome{Turns: 1, Checks: []proseCheck{{Turn: 1, MS: 600, Tokens: 460}}}},
		// The first turn's reply was held back and checked again; the second turn's passed.
		{Task: "answer-01", Try: 3, Outcome: tryOutcome{Turns: 2, Checks: []proseCheck{
			{Turn: 1, MS: 500, Tokens: 400}, {Turn: 1, MS: 900, Tokens: 410}, {Turn: 2, MS: 300, Tokens: 300}}}},
		{Task: "status-01", Try: 1, Outcome: tryOutcome{Turns: 1, Checks: []proseCheck{{Turn: 1, MS: 800, Tokens: 520}}}},
		{Task: "status-01", Try: 2, Outcome: tryOutcome{Turns: 1}},
		{Task: "change-01", Try: 1, Outcome: tryOutcome{Turns: 2}},
	}
	var report strings.Builder
	writeReport(&report, reportInput{options: runOptions{tries: 3, parallel: 1}, all: tasks, tasks: tasks, results: results})
	text := report.String()
	rows := []string{
		"| answer | 4 | 4 | 5 | 0 | 0 | 400 | 1400 |\n",
		"| status | 2 | 1 | 1 | 0 | 0 | 800 | 800 |\n",
		"| find | 1 | 1 | 1 | 0 | 0 | 700 | 700 |\n",
		"| change | 2 | 0 | 0 | 0 | 0 | none | none |\n",
		"| all | 9 | 6 | 7 | 0 | 0 | 600 | 1400 |\n",
	}
	for _, held := range append([]string{"## The no-plan guard's checks", "The run made 7 check calls, which spent 3040 tokens, and skipped the check of 0 turns."}, rows...) {
		if !strings.Contains(text, held) {
			t.Errorf("the report holds no %q:\n%s", held, text)
		}
	}
	for i := 1; i < len(rows); i++ {
		if strings.Index(text, rows[i-1]) > strings.Index(text, rows[i]) {
			t.Errorf("the report gives %q after %q, want answer and status first, then the kinds in eval.Kinds' order, then all:\n%s",
				strings.TrimSpace(rows[i-1]), strings.TrimSpace(rows[i]), text)
		}
	}
}

// TestTheReportCountsTheGuardsSkipsAndTheTurnsJevGaveNoLine: read from fixed reply frames, the
// report counts for each kind the turns whose check the guard skipped, since Jev read their words
// as Ask, which add nothing to the median and the p99, and the turns the guard decided on with no
// line from Jev - none, or a line call still out - which it checked; a turn of a Do reading is
// checked and read. So a run whose server names no decision model shows its turns with no line,
// rather than reading as a run where Jev read no turn as Ask.
func TestTheReportCountsTheGuardsSkipsAndTheTurnsJevGaveNoLine(t *testing.T) {
	tasks := []eval.Task{
		{ID: "answer-01", Kind: eval.KindAnswer, Level: eval.LevelFast},
		{ID: "status-01", Kind: eval.KindStatus, Level: eval.LevelFast},
		{ID: "change-01", Kind: eval.KindChange, Level: eval.LevelFast},
	}
	const end = `{"state":"done","reason":"","offers":[]}`
	tries := []struct {
		task   string
		frames [][2]string
	}{
		{"answer-01", [][2]string{{"reply", `{"step":1,"text":"ACME-7 is open.","check_skipped":"jev_read_ask","jev_line":"ask"}`}, {"end", end}}},
		{"answer-01", [][2]string{{"reply", `{"step":1,"text":"ACME-7 is open.","check_ms":3400,"check_tokens":500,"jev_line":"late"}`}, {"end", end}}},
		{"status-01", [][2]string{{"reply", `{"step":1,"text":"ACME is on track.","check_skipped":"jev_read_ask","jev_line":"ask"}`}, {"end", end}}},
		{"status-01", [][2]string{{"reply", `{"step":1,"text":"ACME is on track.","check_ms":3200,"check_tokens":480,"jev_line":"none"}`}, {"end", end}}},
		{"change-01", [][2]string{
			{"reply", `{"step":1,"text":"","no_plan":true,"check_ms":2900,"check_tokens":510,"jev_line":"do"}`},
			{"reply", `{"step":2,"text":"","tools":["propose_plan"]}`},
			{"end", `{"state":"waiting","reason":"","offers":[]}`},
		}},
	}
	var results []tryResult
	for i, try := range tries {
		var outcome tryOutcome
		for _, frame := range try.frames {
			outcome.readFrame(frame[0], []byte(frame[1]))
		}
		results = append(results, tryResult{Task: try.task, Try: i + 1, Outcome: outcome})
	}
	var report strings.Builder
	writeReport(&report, reportInput{options: runOptions{tries: 2, parallel: 1}, all: tasks, tasks: tasks, results: results})
	text := report.String()
	for _, held := range []string{
		"| kind | turns | turns checked | check calls | turns skipped | turns with no line from Jev | median ms | p99 ms |\n",
		"| answer | 2 | 1 | 1 | 1 | 1 | 3400 | 3400 |\n",
		"| status | 2 | 1 | 1 | 1 | 1 | 3200 | 3200 |\n",
		"| change | 1 | 1 | 1 | 0 | 0 | 2900 | 2900 |\n",
		"| all | 5 | 3 | 3 | 2 | 2 | 3200 | 3400 |\n",
		"The run made 3 check calls, which spent 1490 tokens, and skipped the check of 2 turns. " +
			"2 turns had no line from Jev when the guard decided, 1 of them with the line call still out.",
	} {
		if !strings.Contains(text, held) {
			t.Errorf("the report holds no %q:\n%s", held, text)
		}
	}
}

// TestReadFrameTakesEachCallsTokensAndEachCheckUnderItsTurn: each reply frame of a run is one model
// call, whose output and reasoning tokens the outcome keeps in order; a reply frame that says a
// check (check_ms, 0 included) adds the check under the turn it was made in, which each end frame
// closes; a reply's prose is answered, trimmed, and a block kept. The try's --answers-out line
// carries all of it, with the try's level and its run's working and pace time.
func TestReadFrameTakesEachCallsTokensAndEachCheckUnderItsTurn(t *testing.T) {
	outcome := tryOutcome{WorkingMS: 41_200, PaceMS: 3_100}
	var prose []string
	for _, frame := range []struct{ name, data string }{
		{"reply", `{"step":1,"text":"","tools":["read"],"output_tokens":120,"reasoning_tokens":80}`},
		{"tool", `{"step":2,"tool":"read"}`},
		{"reply", `{"step":3,"text":"","no_plan":true,"output_tokens":40,"reasoning_tokens":30,"check_ms":512,"check_tokens":600}`},
		{"reply", `{"step":4,"text":" ACME-7 is closed. ","output_tokens":60,"reasoning_tokens":20,"check_ms":0,"check_tokens":580}`},
		{"end", `{"state":"done","reason":"","offers":[]}`},
		{"reply", `{"step":5,"text":"Here is the chart.","output_tokens":90,"reasoning_tokens":10,"check_ms":700,"check_tokens":610}`},
		{"block", `{"kind":"chart"}`},
		{"end", `{"state":"done","reason":"","offers":[]}`},
	} {
		if text, _ := outcome.readFrame(frame.name, []byte(frame.data)); text != "" {
			prose = append(prose, text)
		}
	}
	wantCalls := []modelCall{{120, 80}, {40, 30}, {60, 20}, {90, 10}}
	wantChecks := []proseCheck{{Turn: 1, MS: 512, Tokens: 600}, {Turn: 1, MS: 0, Tokens: 580}, {Turn: 2, MS: 700, Tokens: 610}}
	if fmt.Sprint(outcome.ModelCalls) != fmt.Sprint(wantCalls) || fmt.Sprint(outcome.Checks) != fmt.Sprint(wantChecks) || outcome.Turns != 2 {
		t.Errorf("the outcome holds the calls %v, the checks %v and %d turns; want %v, %v and 2", outcome.ModelCalls, outcome.Checks, outcome.Turns, wantCalls, wantChecks)
	}
	if strings.Join(prose, "|") != "ACME-7 is closed.|Here is the chart." || len(outcome.Blocks) != 1 {
		t.Errorf("the frames answered the prose %q and %d blocks; want the two replies' prose, trimmed, and the one block", prose, len(outcome.Blocks))
	}

	line, err := json.Marshal(tryResult{Task: "answer-01", Kind: eval.KindAnswer, Level: eval.LevelFast, Try: 1, Outcome: outcome})
	if err != nil {
		t.Fatal(err)
	}
	for _, held := range []string{
		`"level":"fast"`,
		`"working_ms":41200`,
		`"pace_ms":3100`,
		`"turns":2`,
		`"model_calls":[{"output_tokens":120,"reasoning_tokens":80},{"output_tokens":40,"reasoning_tokens":30},`,
		`"checks":[{"turn":1,"ms":512,"tokens":600},{"turn":1,"ms":0,"tokens":580},{"turn":2,"ms":700,"tokens":610}]`,
	} {
		if !strings.Contains(string(line), held) {
			t.Errorf("the --answers-out line %s holds no %s", line, held)
		}
	}
}
