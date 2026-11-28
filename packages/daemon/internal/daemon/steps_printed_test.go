// The reads the live proofs make of what `dispatch steps --issue <KEY>` prints (step-records.sh,
// owner-steps.sh and experiment.sh run their drivers, built with -tags e2e), and the tests that
// hold them against answers written as the command prints them. The file is untagged so `go test`
// runs these tests on every run: CI builds the tagged drivers with `go vet -tags e2e` and runs no
// test under that tag, so a read that breaks fails this package's tests here, not the live proof
// the next time someone runs it by hand.
package daemon

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stepsPrinted is what `dispatch steps` printed of an issue in no experiment's group, which every
// issue these proofs read is: its header's counts and one line per run, with no `Experiment …` line.
type stepsPrinted struct {
	steps, tasks int
	lines        []stepLine
}

// stepLine is one run as `dispatch steps` prints it: `<task> <step> <kind> <role> <type|->
// attempt <a> <agent|-> <model|-> <started_at|-> <ended_at|running> <result|running> set
// <set model|-> answered <answers>`. The model is the one that answered last, and the answers
// are `running`, `unknown`, `none`, or each model that answered as `<model>=<calls>(<share>%)`.
type stepLine struct {
	text                                 string
	task, kind, role, typ                string
	step, attempt                        int
	agent, model, started, ended, result string
	setModel, answered                   string
}

// parseSteps reads the output of `dispatch steps --issue key` for an issue in no experiment's
// group. That is the header `<key>: <n> steps in <m> tasks`, then exactly n lines of runs
// (runFields). An `Experiment <experiment>: variant <variant>.` line between them is no run.
// The line of a task of the issue's plan, `task <task> needs <needs>: …` with its needs as
// `[a,b]`, `[]` or `-`, which comes before that task's runs, is no run and is passed over.
func parseSteps(key, out string) (stepsPrinted, error) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	var printed stepsPrinted
	header := strings.TrimPrefix(lines[0], key+": ")
	if header == lines[0] {
		return printed, fmt.Errorf("the first line %q is no header of %s", lines[0], key)
	}
	if _, err := fmt.Sscanf(header, "%d steps in %d tasks", &printed.steps, &printed.tasks); err != nil {
		return printed, fmt.Errorf("the header %q: %v", lines[0], err)
	}
	for _, text := range lines[1:] {
		if fields := strings.Fields(text); len(fields) > 2 && fields[0] == "task" && fields[2] == "needs" {
			continue
		}
		fields, ok := runFields(text)
		if !ok {
			return printed, fmt.Errorf("the line %q is no run", text)
		}
		step, err := strconv.Atoi(fields[1])
		if err != nil {
			return printed, fmt.Errorf("the line %q: step %v", text, err)
		}
		attempt, err := strconv.Atoi(fields[6])
		if err != nil {
			return printed, fmt.Errorf("the line %q: attempt %v", text, err)
		}
		printed.lines = append(printed.lines, stepLine{
			text: text, task: fields[0], step: step, kind: fields[2], role: fields[3], typ: fields[4], attempt: attempt,
			agent: fields[7], model: fields[8], started: fields[9], ended: fields[10], result: fields[11],
			setModel: fields[13], answered: fields[15],
		})
	}
	if len(printed.lines) != printed.steps {
		return printed, fmt.Errorf("the header says %d steps and %d lines follow it", printed.steps, len(printed.lines))
	}
	return printed, nil
}

// variantPlace answers nil where line stands in printed, the stdout of `dispatch steps --issue
// key`, where the command prints the variant of the issue: right after the header and before the
// first run. It checks that place alone. The rest of the answer, with the line taken out, must read
// as parseSteps reads the command, with at least one run. It answers where the line stands
// otherwise.
func variantPlace(key, line, printed string) error {
	lines := strings.Split(strings.TrimRight(printed, "\n"), "\n")
	if at := slices.Index(lines, line); at != 1 {
		if at < 1 {
			return fmt.Errorf("no line below the header is %q", line)
		}
		return fmt.Errorf("it is line %d of %d, after %q; want it right after the header and before the first run", at+1, len(lines), lines[at-1])
	}
	rest, err := parseSteps(key, strings.Join(slices.Delete(slices.Clone(lines), 1, 2), "\n"))
	if err != nil {
		return err
	}
	if rest.steps == 0 {
		return fmt.Errorf("the header of %s says 0 steps, so no run follows the line", key)
	}
	return nil
}

// holdsTen says whether printed is the walk's read: exactly the ten runs of stepRecordsTen, under
// the header `10 steps in 1 tasks`.
func holdsTen(printed stepsPrinted) error {
	if err := holdsLines(printed.lines, stepRecordsTen, true); err != nil {
		return err
	}
	if printed.steps != len(stepRecordsTen) || printed.tasks != 1 {
		return fmt.Errorf("the header says %d steps in %d tasks, want %d steps in 1 tasks", printed.steps, printed.tasks, len(stepRecordsTen))
	}
	return nil
}

// holdsLines says whether lines hold each run of want, of the task default, ended (its end a
// moment, printedMoment) with its result, with an agent, and each of kind step with the model that
// answered it last. Each holds the set model and the answers the walk's stand-ins leave it
// (printedModels). With exact, they hold no other run. The error names the first run that is
// missing or wrong as lineName spells it.
func holdsLines(lines []stepLine, want []stepWant, exact bool) error {
	for _, w := range want {
		got, name, typ := findLine(lines, w), lineName(w), printedType(w)
		setModel, answered := printedModels(w)
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "default" || got.role != w.role || got.typ != typ || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task default, role %s, type %s and an agent", name, got.text, w.role, typ)
		case got.started == "-" || !printedMoment(got.ended) || got.result != w.result:
			return fmt.Errorf("%s prints %q: want a start, an end and result %s", name, got.text, w.result)
		case w.kind == "step" && got.model != stepRecordsModel:
			return fmt.Errorf("%s prints %q: want the model %s", name, got.text, stepRecordsModel)
		case got.setModel != setModel || got.answered != answered:
			return fmt.Errorf("%s prints %q: want set %s answered %s", name, got.text, setModel, answered)
		}
	}
	if exact && len(lines) != len(want) {
		return fmt.Errorf("%d lines of runs, want %d", len(lines), len(want))
	}
	return nil
}

// printedMoment is whether s, a column of `dispatch steps`, is a moment as Dispatch writes one
// (RFC 3339): an end is one, and `running`, which steps.ts prints for a run that has not ended, or
// any other text, is none.
func printedMoment(s string) bool {
	_, err := time.Parse(time.RFC3339Nano, s)
	return err == nil
}

// findLine is the line of w's kind, step and attempt, the zero line when lines hold none.
func findLine(lines []stepLine, w stepWant) stepLine {
	for _, line := range lines {
		if line.kind == w.kind && line.step == w.step && line.attempt == w.attempt {
			return line
		}
	}
	return stepLine{}
}

// printedType is w's type as the command prints it: `-` for the retro and the merge, which have
// none.
func printedType(w stepWant) string {
	if w.typ == "" {
		return "-"
	}
	return w.typ
}

// printedModels is what `dispatch steps` must print of w's set model and of the models that
// answered it, as the walk's stand-ins leave them. Each agent of the walk answers once in each of
// its runs, on the one model its role was set to (stepRecordsModel). So a step or the retro prints
// that model as its set model and its one call on it. The merger the merge launches never
// registers, so no read of its calls is answered. The merge then prints no set model, and that its
// counts are not known.
func printedModels(w stepWant) (setModel, answered string) {
	if w.kind == "merge" {
		return "-", "unknown"
	}
	return stepRecordsModel, stepRecordsModel + "=1(100%)"
}

// lineName spells w as the command prints its leading fields, then its result: `4 step reviewer
// review attempt 1 … failed`.
func lineName(w stepWant) string {
	return fmt.Sprintf("%d %s %s %s attempt %d … %s", w.step, w.kind, w.role, printedType(w), w.attempt, w.result)
}

// walkPrinted is what `dispatch steps --issue SR-1` prints of the walk's ten runs (stepRecordsTen).
// Each run ended at a moment with its result. Its set model and its answers are as the walk's
// stand-ins leave them (printedModels). Each line is the words edit returns of the line's run and
// of its 16 words.
func walkPrinted(edit func(w stepWant, words []string) []string) string {
	lines := []string{"SR-1: 10 steps in 1 tasks"}
	for _, w := range stepRecordsTen {
		model := "-"
		if w.kind == "step" {
			model = stepRecordsModel
		}
		setModel, answered := printedModels(w)
		words := []string{"default", strconv.Itoa(w.step), w.kind, w.role, printedType(w), "attempt", strconv.Itoa(w.attempt), "agent-" + w.role,
			model, "2026-11-20T09:00:00Z", "2026-11-20T09:40:00.123456Z", w.result, "set", setModel, "answered", answered}
		lines = append(lines, strings.Join(edit(w, words), " "))
	}
	return strings.Join(lines, "\n") + "\n"
}

// The driver's end check, which the proof runs over what `dispatch steps` prints: a line ends its
// run only when it prints a moment as its end. steps.ts prints `running` there, as the result and
// as the answers, for a run that has not ended. `-`, which a printer that wrote the end through
// orDash would print, and any other word are no end either. So each fails the walk's read, naming
// the line. The control is the same ten lines with every end a moment, which the check passes. A
// line of the command before the set model, 12 words that end at the result, is no run, and the
// parse fails naming it.
func TestTheDriverReadsAnEndOnlyFromAMoment(t *testing.T) {
	t.Parallel()
	ending := func(end, result string) func(stepWant, []string) []string {
		return func(w stepWant, words []string) []string {
			if w.kind == "step" && w.step == 2 && w.attempt == 2 {
				words[10], words[11] = end, result
				if end == "running" {
					words[15] = "running"
				}
			}
			return words
		}
	}
	read := func(out string) error {
		parsed, err := parseSteps("SR-1", out)
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return holdsTen(parsed)
	}
	if err := read(walkPrinted(ending("2026-11-20T09:40:00Z", "passed"))); err != nil {
		t.Fatalf("control: the ten lines, each ended at a moment, read as %v, want the walk's ten", err)
	}
	for _, no := range []struct{ end, result string }{{"running", "running"}, {"-", "passed"}, {"unknown", "passed"}} {
		err := read(walkPrinted(ending(no.end, no.result)))
		if err == nil || !strings.Contains(err.Error(), "2 step implementer implementation attempt 2") || !strings.Contains(err.Error(), "an end") {
			t.Errorf("the ten lines with implementing's second run ending %q %q read as %v, want an error naming that run and its end", no.end, no.result, err)
		}
	}
	old := walkPrinted(func(_ stepWant, words []string) []string { return words[:12] })
	first := "default 1 step planner planning attempt 1 agent-planner stand-in/model 2026-11-20T09:00:00Z 2026-11-20T09:40:00.123456Z passed"
	if parsed, err := parseSteps("SR-1", old); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("the line %q is no run", first)) {
		t.Errorf("the ten lines in the 12 words of the command before the set model parsed as %+v, %v; want an error naming %q", parsed, err, first)
	}
}

// The driver's check of each run's models. A step or the retro prints the stand-in's model as its
// set model, and its one call on it. The merge, whose agent never registers, prints no set model
// and that its counts are not known (printedModels). The ten lines as the walk leaves them pass.
// Each control prints one run's models otherwise, and wants the error that names that run.
func TestTheDriverReadsEachRunsSetModelAndAnswers(t *testing.T) {
	t.Parallel()
	parsed, err := parseSteps("SR-1", walkPrinted(func(_ stepWant, words []string) []string { return words }))
	if err != nil {
		t.Fatal(err)
	}
	if err := holdsTen(parsed); err != nil {
		t.Fatalf("control: the ten lines as the walk leaves them read as %v, want the walk's ten", err)
	}
	for _, control := range []struct {
		name, kind    string
		step, attempt int
		set, answered string
		why           string
	}{
		{"implementing's second run counted with its first", "step", 2, 2, stepRecordsModel, stepRecordsModel + "=2(100%)", "2 step implementer implementation attempt 2"},
		{"a review answered off its set model", "step", 4, 1, stepRecordsModel, stepRecordsModel + "=1(50%),stand-in/other=1(50%)", "4 step reviewer review attempt 1"},
		{"a test that does not know its counts", "step", 3, 1, stepRecordsModel, "unknown", "3 step tester testing attempt 1"},
		{"the retro with no set model", "retro", 0, 1, "-", stepRecordsModel + "=1(100%)", "0 retro implementer - attempt 1"},
		{"the merge with a set model", "merge", 0, 1, stepRecordsModel, "unknown", "0 merge merger - attempt 1"},
	} {
		out := walkPrinted(func(w stepWant, words []string) []string {
			if w.kind == control.kind && w.step == control.step && w.attempt == control.attempt {
				words[13], words[15] = control.set, control.answered
			}
			return words
		})
		parsed, err := parseSteps("SR-1", out)
		if err != nil {
			t.Fatalf("%s: parse: %v", control.name, err)
		}
		if err := holdsTen(parsed); err == nil || !strings.Contains(err.Error(), control.why) || !strings.Contains(err.Error(), "want set") {
			t.Errorf("control: %s read as %v, want an error naming %q and the set model and answers it wants", control.name, err, control.why)
		}
	}
}

// variantPlace reads each run below the Experiment line as `dispatch steps` prints it now: 16
// words that end `set <set model> answered <answers>`. The walk's ten runs below the line pass.
// The same runs in the 12 words of the command before the set model are no runs, and the place
// fails naming the first.
func TestVariantPlaceReadsEachRunInTheWordsDispatchStepsPrints(t *testing.T) {
	t.Parallel()
	const line = "Experiment planning: variant b."
	withLine := func(printed string) string {
		header, runs, _ := strings.Cut(printed, "\n")
		return header + "\n" + line + "\n" + runs
	}
	if err := variantPlace("SR-1", line, withLine(walkPrinted(func(_ stepWant, words []string) []string { return words }))); err != nil {
		t.Errorf("the ten runs of 16 words below %q read as %v, want the line in its place", line, err)
	}
	old := walkPrinted(func(_ stepWant, words []string) []string { return words[:12] })
	first := strings.SplitN(old, "\n", 3)[1]
	if err := variantPlace("SR-1", line, withLine(old)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("the line %q is no run", first)) {
		t.Errorf("the ten runs in the 12 words of the command before the set model read as %v, want an error naming %q", err, first)
	}
}

// variantPlace reads `dispatch steps` as this release prints it. It holds the variant's line right
// after the header, and parseSteps reads the rest: the line of each task of the issue's plan,
// `task <task> needs <needs>: …`, stands before that task's runs and is no run, and the header's
// count is the runs below it. The fault is a read that takes the task line for a run, which fails
// the first row.
func TestVariantPlacePassesOverTheLineOfAPlansTask(t *testing.T) {
	t.Parallel()
	const (
		run     = "default 1 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed set anthropic/claude-opus-5 answered anthropic/claude-opus-5=1(100%)"
		variant = "Experiment planning: variant b."
		task    = "task default needs []: 1 implementer passed"
	)
	for _, row := range []struct {
		name, printed string
		ok            bool
	}{
		{"the task line below the variant", "LEG-1: 1 steps in 1 tasks\n" + variant + "\n" + task + "\n" + run + "\n", true},
		{"the variant below the task line", "LEG-1: 1 steps in 1 tasks\n" + task + "\n" + variant + "\n" + run + "\n", false},
		{"a header that counts the task line as a run", "LEG-1: 2 steps in 1 tasks\n" + variant + "\n" + task + "\n" + run + "\n", false},
		{"a line that is neither a run nor a task line", "LEG-1: 1 steps in 1 tasks\n" + variant + "\ntask default 1 implementer\n" + run + "\n", false},
		{"a header of another issue", "LEG-2: 1 steps in 1 tasks\n" + variant + "\n" + task + "\n" + run + "\n", false},
		{"a header of no run", "LEG-1: 0 steps in 1 tasks\n" + variant + "\n" + task + "\n", false},
	} {
		t.Run(row.name, func(t *testing.T) {
			if err := variantPlace("LEG-1", variant, row.printed); (err == nil) != row.ok {
				t.Fatalf("variantPlace of\n%s= %v, want it to hold: %v", row.printed, err, row.ok)
			}
		})
	}
}
