package supervise

// The cost reading (costreading.go): the documentation site's page and the sample log committed
// beside this package hold each other to the reader, the reader reads what the machine itself
// writes, and each of R9's verdicts lists what fails it.

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/testlog"
)

const (
	// taskCostPage is the documentation site's page of the reading, from this package's directory:
	// the repository root is four directories up.
	taskCostPage = "../../../../docs/site/src/content/docs/legion/task-delivery-cost.md"
	// taskCostSample is the sample log the page's examples read, from this package's directory,
	// and taskCostSampleFromRoot the same file as the page names it, from the repository root.
	taskCostSample         = "testdata/task-cost.log"
	taskCostSampleFromRoot = "packages/daemon/internal/supervise/" + taskCostSample
	// taskCostCommand is the command the page gives in one fenced block.
	taskCostCommand = "kubectl -n legion logs deploy/legion-<project>-daemon -c legion --since-time <from> | legion task-cost --from <from> --to <to> [--claim <token>]"
)

// docExample is one example of the page: the arguments its fenced `legion task-cost` command
// gives, and the output the page shows for it in the fenced block after it.
type docExample struct {
	command string
	args    []string
	output  string
}

// fencedBlock is one fenced code block of a Markdown page: its language and its text.
type fencedBlock struct {
	lang, text string
}

// fencedBlocks is every fenced code block of page, in order.
func fencedBlocks(page string) []fencedBlock {
	var blocks []fencedBlock
	var open *fencedBlock
	var body []string
	for _, line := range strings.Split(page, "\n") {
		switch {
		case open == nil && strings.HasPrefix(line, "```"):
			open, body = &fencedBlock{lang: strings.TrimPrefix(line, "```")}, nil
		case open != nil && line == "```":
			open.text = strings.Join(body, "\n") + "\n"
			blocks = append(blocks, *open)
			open = nil
		case open != nil:
			body = append(body, line)
		}
	}
	return blocks
}

// taskCostExamples reads the page: it fails unless the page gives the reading's command in one
// fenced block, and answers each example — a fenced `legion task-cost` command reading the sample,
// followed by a fenced block of its output.
func taskCostExamples(t *testing.T) []docExample {
	t.Helper()
	page, err := os.ReadFile(taskCostPage)
	if err != nil {
		t.Fatalf("read the page: %v", err)
	}
	blocks := fencedBlocks(string(page))
	commands := 0
	var examples []docExample
	for i, block := range blocks {
		if block.lang == "sh" && block.text == taskCostCommand+"\n" {
			commands++
		}
		line, ok := strings.CutPrefix(strings.TrimSuffix(block.text, "\n"), "legion task-cost ")
		if block.lang != "sh" || !ok {
			continue
		}
		args, input, redirected := strings.Cut(line, " < ")
		if !redirected || input != taskCostSampleFromRoot {
			t.Fatalf("the page's example %q reads %q, want the sample %s on its standard input", block.text, input, taskCostSampleFromRoot)
		}
		if i+1 == len(blocks) || blocks[i+1].lang != "text" {
			t.Fatalf("the page's example %q is followed by no fenced text block of its output", block.text)
		}
		examples = append(examples, docExample{command: line, args: strings.Fields(args), output: blocks[i+1].text})
	}
	if commands != 1 {
		t.Fatalf("the page gives %q in %d fenced blocks, want one", taskCostCommand, commands)
	}
	claims := 0
	for _, example := range examples {
		if slices.Contains(example.args, "--claim") {
			claims++
		}
	}
	if claims == 0 || claims == len(examples) {
		t.Fatalf("the page's examples %q read the sample whole %d times and one claim %d times, want both", examples, len(examples)-claims, claims)
	}
	return examples
}

// readSample is the sample log the page's examples read (taskCostSampleFromRoot names the same
// file from the repository root).
func readSample(t *testing.T) []byte {
	t.Helper()
	sample, err := os.ReadFile(taskCostSample)
	if err != nil {
		t.Fatalf("read the sample: %v", err)
	}
	return sample
}

// readExample runs the example's command over log, as `legion task-cost` runs it.
func readExample(t *testing.T, example docExample, log []byte) TaskCost {
	t.Helper()
	args, err := ParseTaskCostArgs(example.args)
	if err != nil {
		t.Fatalf("the page's example %q: %v", example.command, err)
	}
	cost, err := ReadTaskCost(strings.NewReader(string(log)), args.From, args.To, args.Claim)
	if err != nil {
		t.Fatalf("read the sample for %q: %v", example.command, err)
	}
	return cost
}

// figure is one figure of the reading's output: its name, and its value — the rest of its line,
// with the indented lines under it (each task of a list, each failure of a verdict).
type figure struct {
	name, value string
}

// figures is the reading's output figure by figure, in order. A figure's name is its line up to
// the first ": ", or a verdict's up to its last, since a verdict's name holds no colon and its
// value is pass or fail; a line that ends in ":" names a figure whose value is the indented lines
// under it.
func figures(text string) []figure {
	var out []figure
	for _, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if strings.HasPrefix(line, "  ") && len(out) > 0 {
			last := &out[len(out)-1]
			last.value = strings.TrimPrefix(last.value+"\n"+strings.TrimSpace(line), "\n")
			continue
		}
		cut := strings.Index(line, ": ")
		if strings.HasPrefix(line, "verdict: ") {
			cut = strings.LastIndex(line, ": ")
		}
		if cut < 0 {
			out = append(out, figure{name: strings.TrimSuffix(line, ":")})
			continue
		}
		out = append(out, figure{name: line[:cut], value: line[cut+2:]})
	}
	return out
}

// figureDiff is every figure where the page's output and the reading's part, or "" where the two
// are the same text: a figure one shows with another value than the other, or that only one of
// them shows, and, where every figure agrees, the text itself.
func figureDiff(want, got string) string {
	gave, showed := map[string]string{}, map[string]bool{}
	for _, f := range figures(got) {
		gave[f.name] = f.value
	}
	var diff []string
	for _, f := range figures(want) {
		showed[f.name] = true
		switch value, ok := gave[f.name]; {
		case !ok:
			diff = append(diff, fmt.Sprintf("%s: the page shows %q, the reading gives no such figure", f.name, f.value))
		case value != f.value:
			diff = append(diff, fmt.Sprintf("%s: the page shows %q, the reading gives %q", f.name, f.value, value))
		}
	}
	for _, f := range figures(got) {
		if !showed[f.name] {
			diff = append(diff, fmt.Sprintf("%s: the reading gives %q, the page shows no such figure", f.name, f.value))
		}
	}
	if len(diff) == 0 && want != got {
		diff = append(diff, fmt.Sprintf("every figure agrees, but the page shows the text %q and the reading gives %q", want, got))
	}
	return strings.Join(diff, "\n")
}

// turnCause is the task attribute of a turn line, which a sample whose turn lines carry no cause
// lacks (TestDocExampleTaskCost's negative control).
var turnCause = regexp.MustCompile(`,"task":\d+`)

// The page's command, run as the page gives it over the sample committed beside this package,
// prints every figure the page shows: for the whole sample and for one claim. The sample holds a
// task written to a claim whose phase waited and a silence repair's write to such a claim, so the
// waited figure and the silence count are not zero, and the resume verdict is seen not to count
// the silence repair's write.
//
// The negative control is the same reading over the sample with the cause taken out of every turn
// line: the figures the page shows do not hold, which is the proof that they come from the turn
// lines' causes. And a window the log does not reach reads no line and no turn at all, so the
// figures come from the lines and from no constant.
func TestDocExampleTaskCost(t *testing.T) {
	examples := taskCostExamples(t)
	sample := readSample(t)

	t.Run("the whole sample and one claim", func(t *testing.T) {
		for _, example := range examples {
			cost := readExample(t, example, sample)
			if diff := figureDiff(example.output, cost.Text()); diff != "" {
				t.Errorf("legion task-cost %s gives figures the page does not show:\n%s", example.command, diff)
			}
			if !slices.Contains(example.args, "--claim") {
				if cost.WaitedByOrigin[OriginSilence] == 0 || cost.WrittenByOrigin[OriginSilence] == 0 {
					t.Errorf("the whole sample reads %v written and %v waited by origin, want the silence repair's write to a waiting claim in both",
						cost.WrittenByOrigin, cost.WaitedByOrigin)
				}
				if len(cost.ResumeToFinished) != 0 {
					t.Errorf("the resume verdict fails on %q, want the silence repair's write left out of it", cost.ResumeToFinished)
				}
			}
		}
	})

	t.Run("a sample whose turn lines carry no cause", func(t *testing.T) {
		var stripped []string
		for _, line := range strings.Split(string(sample), "\n") {
			if strings.Contains(line, `"msg":"`+startedLine+`"`) || strings.Contains(line, `"msg":"`+endedLine+`"`) {
				line = turnCause.ReplaceAllString(line, "")
			}
			stripped = append(stripped, line)
		}
		uncaused := []byte(strings.Join(stripped, "\n"))
		for _, example := range examples {
			diff := figureDiff(example.output, readExample(t, example, uncaused).Text())
			if diff == "" {
				t.Fatalf("legion task-cost %s over turn lines with no cause gives the page's figures, want them not to hold", example.command)
			}
			t.Logf("legion task-cost %s over turn lines with no cause does not give the page's figures:\n%s", example.command, diff)
		}
	})

	t.Run("a window the log does not reach", func(t *testing.T) {
		for _, example := range examples {
			args, err := ParseTaskCostArgs(example.args)
			if err != nil {
				t.Fatal(err)
			}
			day := 24 * time.Hour
			cost, err := ReadTaskCost(strings.NewReader(string(sample)), args.From.Add(-day), args.To.Add(-day), args.Claim)
			if err != nil {
				t.Fatal(err)
			}
			if cost.Lines != 0 || cost.TurnsStarted != (TurnCauses{}) || cost.TurnsEnded != (TurnCauses{}) || cost.Taken != 0 {
				t.Fatalf("a day before the sample reads %d lines, turns %+v and %+v, %d taken; want nothing", cost.Lines,
					cost.TurnsStarted, cost.TurnsEnded, cost.Taken)
			}
		}
	})
}

// jsonHarness is waitsHarness with the machine's log written as the daemon writes it — slog's
// JSON handler at its default level (cmd/legion's start) — into log, each line stamped with the
// harness clock's time rather than the wall clock's, so a window reads the walk.
func jsonHarness(t *testing.T) (*harness, *testlog.Buffer) {
	t.Helper()
	h := newBareHarness(t)
	withWaits(&h.deps)
	log := &testlog.Buffer{}
	h.deps.Log = slog.New(logRecorder{
		Handler: slog.NewJSONHandler(log, &slog.HandlerOptions{ReplaceAttr: func(groups []string, a slog.Attr) slog.Attr {
			if len(groups) == 0 && a.Key == slog.TimeKey {
				return slog.Time(slog.TimeKey, h.clock.Now())
			}
			return a
		}}),
		records: h.records,
	})
	if err := h.store.PutClaim(h.ctx, queuedClaim()); err != nil {
		t.Fatal(err)
	}
	h.start(queuedClaim())
	return h, log
}

// keysByMessage is, for each message of the JSON lines in log, every attribute key a line of it
// carries.
func keysByMessage(t *testing.T, log string) map[string]map[string]bool {
	t.Helper()
	keys := map[string]map[string]bool{}
	for _, line := range strings.Split(log, "\n") {
		var fields map[string]any
		if !strings.HasPrefix(line, "{") || json.Unmarshal([]byte(line), &fields) != nil {
			continue
		}
		msg, _ := fields["msg"].(string)
		if keys[msg] == nil {
			keys[msg] = map[string]bool{}
		}
		for key := range fields {
			keys[msg][key] = true
		}
	}
	return keys
}

// The reader reads the lines the machine itself writes, not only the sample written to look like
// them: a walk of a claim through a take answered twice, the take's turn, a turn of the worker's
// own, a resume written to a claim that had finished its last task in its session, and a take
// whose turn did not start within the second bound, logged through the daemon's own handler, reads
// back as those things. Every line of the sample carries only keys the machine writes on a line
// of that message.
func TestTaskCostReadsTheMachinesOwnLog(t *testing.T) {
	h, log := jsonHarness(t)
	h.reach(StateReady)
	h.must(RequestDeliver{Claim: testToken, Task: "implement the plan", ID: "outbox:1", Row: 1,
		Phase: phase.Implementing, Generation: 1})
	value, _ := h.values.value(testToken)
	if again, err := h.takeValue(value); err != nil || again.Outcome != TakeOutcomeAccepted {
		t.Fatalf("the take sent again = %+v, %v; want it answered accepted from the record", again, err)
	}
	h.advance(30 * time.Second)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(10 * time.Minute)
	h.must(StreamTurnEnd{Claim: testToken})
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.mustQuiet(StreamTurnEnd{Claim: testToken})
	h.advance(time.Minute)
	h.must(RequestDeliver{Claim: testToken, Task: "carry on with the plan", ID: "outbox:2", Row: 2,
		Phase: phase.Implementing, Generation: 1, Origin: OriginResume})
	h.advance(turnStartBound)
	h.advance(time.Minute)
	h.must(StreamTurnStart{Claim: testToken})
	h.advance(5 * time.Minute)
	h.must(StreamTurnEnd{Claim: testToken})
	if p := h.claim().Pending; p != nil {
		t.Fatalf("pending %+v, want both tasks finished", p)
	}

	cost, err := ReadTaskCost(strings.NewReader(log.String()), epoch.Add(-time.Hour), h.clock.Now().Add(time.Second), "")
	if err != nil {
		t.Fatal(err)
	}
	if cost.TurnsStarted != (TurnCauses{Task: 2, None: 1}) || cost.TurnsEnded != (TurnCauses{Task: 2, None: 1}) {
		t.Errorf("turns started %+v and ended %+v, want two by a task number and one with no cause each", cost.TurnsStarted, cost.TurnsEnded)
	}
	if want := []TaskTurns{{string(testToken), 1, 1}, {string(testToken), 2, 1}}; !slices.Equal(cost.TurnsOfTask, want) {
		t.Errorf("turns of each number %+v, want %+v", cost.TurnsOfTask, want)
	}
	if want := map[string]int{OriginPhaseStart: 1, OriginResume: 1}; !maps.Equal(cost.WrittenByOrigin, want) {
		t.Errorf("numbers written by origin %v, want %v", cost.WrittenByOrigin, want)
	}
	if want := map[string]int{OriginResume: 1}; !maps.Equal(cost.WaitedByOrigin, want) {
		t.Errorf("numbers written to a waiting claim by origin %v, want %v", cost.WaitedByOrigin, want)
	}
	if cost.Taken != 2 || cost.Finished != 2 || !maps.Equal(cost.FinishedBy, map[string]int{"report": 2}) {
		t.Errorf("taken %d, finished %d by %v; want two of each, both by the report", cost.Taken, cost.Finished, cost.FinishedBy)
	}
	if !maps.Equal(cost.ChargesByWait, map[string]int{WaitTurnStart: 1}) || cost.Retirements != 0 {
		t.Errorf("charges by wait %v and %d retirements, want the one charge of the second wait", cost.ChargesByWait, cost.Retirements)
	}
	if len(cost.ChargedWithinBound) != 0 || len(cost.TakenPastOwed) != 0 {
		t.Errorf("the first verdict fails on %q and the third on %q, want both to pass", cost.ChargedWithinBound, cost.TakenPastOwed)
	}
	if len(cost.ResumeToFinished) != 1 || !strings.HasPrefix(cost.ResumeToFinished[0], string(testToken)+" task 2:") {
		t.Errorf("the resume verdict fails on %q, want task 2 alone", cost.ResumeToFinished)
	}

	machine := keysByMessage(t, log.String())
	sample := keysByMessage(t, string(readSample(t)))
	for msg := range costedLines {
		if sample[msg] == nil {
			continue
		}
		if machine[msg] == nil {
			t.Errorf("the walk wrote no %q, so the sample's lines of it are held to nothing", msg)
			continue
		}
		for key := range sample[msg] {
			if !machine[msg][key] {
				t.Errorf("the sample's %q lines carry %q, which the machine never writes on that line", msg, key)
			}
		}
	}
}

// costLogLine is one line of the daemon's log, as slog's JSON handler writes it, for a reading
// built line by line.
func costLogLine(t *testing.T, at time.Time, msg, claim string, attrs ...any) string {
	t.Helper()
	fields := map[string]any{"time": at.Format("2006-01-02T15:04:05.000Z07:00"), "level": "INFO", "msg": msg, "claim": claim}
	for i := 0; i+1 < len(attrs); i += 2 {
		fields[attrs[i].(string)] = attrs[i+1]
	}
	line, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(line)
}

// Each of R9's verdicts fails where the log shows what it forbids, and lists it: a charge of a
// worker whose turn began within the second bound after its take, a resume written to a claim
// that had finished its last task in its session, and a number taken twice for one time owed. A
// repeated answer from the record is no second take, a take-back opens a second time owed, and a
// silence repair's write to a waiting claim is no resume.
func TestTaskCostLogVerdictsListWhatFailsThem(t *testing.T) {
	const claim = "legion-infra-infra-2811-tester"
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	written := func(offset time.Duration, task uint64, origin string, finished bool) string {
		return costLogLine(t, at.Add(offset), valueWrittenLine, claim, "task", task, "origin", origin, "finished", finished, "cause", TaskValueQueued)
	}
	accepted := func(offset time.Duration, task uint64, repeat bool) string {
		return costLogLine(t, at.Add(offset), takeAnsweredLine, claim, "task", task, "outcome", "accepted", "hold", "", "repeat", repeat)
	}
	lines := []string{
		// Task 1: an accepted take charged five seconds after it, inside its bound, whose turn
		// began thirty seconds after it.
		written(0, 1, OriginPhaseStart, false),
		accepted(time.Second, 1, false),
		costLogLine(t, at.Add(6*time.Second), promptFailureLine, claim, "wait", WaitTurnStart, "bound", int64(turnStartBound)),
		costLogLine(t, at.Add(6*time.Second), takenBackLine, claim, "task", 1, "cause", WaitTurnStart),
		costLogLine(t, at.Add(31*time.Second), turnStartedLine, claim, "generation", 1),
		costLogLine(t, at.Add(time.Minute), turnEndedLine, claim, "generation", 1),
		// Task 2: written to a claim whose phase waited, as a resume, and taken twice with nothing
		// taken back between, beside a repeated answer.
		written(2*time.Minute, 2, OriginResume, true),
		accepted(2*time.Minute+time.Second, 2, false),
		accepted(2*time.Minute+2*time.Second, 2, true),
		accepted(2*time.Minute+3*time.Second, 2, false),
		// Task 3: the silence repair's write to a waiting claim, taken twice around a take-back.
		written(3*time.Minute, 3, OriginSilence, true),
		accepted(3*time.Minute+time.Second, 3, false),
		costLogLine(t, at.Add(3*time.Minute+2*time.Second), takenBackLine, claim, "task", 3, "cause", "compaction"),
		accepted(3*time.Minute+3*time.Second, 3, false),
	}
	cost, err := ReadTaskCost(strings.NewReader(strings.Join(lines, "\n")+"\n"), at, at.Add(time.Hour), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(cost.ChargedWithinBound) != 1 || !strings.HasPrefix(cost.ChargedWithinBound[0], claim+" task 1: taken 2026-11-14T09:00:01.000Z, a turn started 2026-11-14T09:00:31.000Z") {
		t.Fatalf("the first verdict fails on %q, want task 1's charge with the turn that began inside its bound", cost.ChargedWithinBound)
	}
	if want := []string{claim + " task 2: written 2026-11-14T09:02:00.000Z"}; !slices.Equal(cost.ResumeToFinished, want) {
		t.Errorf("the resume verdict fails on %q, want %q", cost.ResumeToFinished, want)
	}
	if want := []string{claim + " task 2: taken 2 times, owed 1"}; !slices.Equal(cost.TakenPastOwed, want) {
		t.Errorf("the third verdict fails on %q, want %q", cost.TakenPastOwed, want)
	}
	text := cost.Text()
	for _, want := range []string{
		"verdict: no charge of a worker whose turn began within the second bound after its take: fail\n  " + cost.ChargedWithinBound[0] + "\n",
		"verdict: no write of origin resume to a claim that finished its last task in its session: fail\n  " + claim + " task 2:",
		"verdict: each number taken once for each time the record owed it: fail\n  " + claim + " task 2: taken 2 times, owed 1\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("the reading prints\n%s\nwhich does not hold %q", text, want)
		}
	}
}

// The reading's arguments are the window, both ends RFC 3339 times with the second after the
// first, and optionally one claim; nothing else is taken, and a help flag is the command's usage.
func TestParseTaskCostArgsReadsTheLogWindow(t *testing.T) {
	from, to := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC), time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		args []string
		want TaskCostArgs
	}{
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z"}, TaskCostArgs{From: from, To: to}},
		{[]string{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "--claim", "legion-infra-infra-2811-tester"},
			TaskCostArgs{From: from, To: to, Claim: "legion-infra-infra-2811-tester"}},
		{[]string{"--to=2026-11-14T10:30:00+01:00", "--from=2026-11-14T09:00:00.000Z"}, TaskCostArgs{From: from, To: to}},
	} {
		got, err := ParseTaskCostArgs(tc.args)
		if err != nil || !got.From.Equal(tc.want.From) || !got.To.Equal(tc.want.To) || got.Claim != tc.want.Claim {
			t.Errorf("ParseTaskCostArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}
	for _, args := range [][]string{
		{"--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:00:00Z"},
		{"--from", "yesterday", "--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:30:00Z", "--to", "2026-11-14T09:30:00Z"},
		{"--from", "2026-11-14T09:00:00Z", "--to", "2026-11-14T09:30:00Z", "daemon.log"},
		{"--since", "30m"},
	} {
		if got, err := ParseTaskCostArgs(args); err == nil || errors.Is(err, flag.ErrHelp) {
			t.Errorf("ParseTaskCostArgs(%q) = %+v, %v; want a refusal", args, got, err)
		}
	}
	for _, help := range []string{"-h", "-help", "--help"} {
		if _, err := ParseTaskCostArgs([]string{help}); !errors.Is(err, flag.ErrHelp) {
			t.Errorf("ParseTaskCostArgs(%q) = %v, want flag.ErrHelp", help, err)
		}
	}
}
