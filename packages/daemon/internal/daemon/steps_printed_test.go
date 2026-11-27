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
// attempt <a> <agent|-> <model|-> <started_at|-> <ended_at|running> <result|running>`.
type stepLine struct {
	text                                 string
	task, kind, role, typ                string
	step, attempt                        int
	agent, model, started, ended, result string
}

// parseSteps reads the output of `dispatch steps --issue key` for an issue in no experiment's
// group: the header `<key>: <n> steps in <m> tasks`, then exactly n lines of runs, with no
// `Experiment <experiment>: variant <variant>.` line between them, which it would read as no run.
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
		fields := strings.Fields(text)
		if len(fields) > 2 && fields[0] == "task" && fields[2] == "needs" {
			continue
		}
		if len(fields) != 12 || fields[5] != "attempt" {
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
// answered it; with exact, they hold no other run. The error names the first run that is missing
// or wrong as lineName spells it.
func holdsLines(lines []stepLine, want []stepWant, exact bool) error {
	for _, w := range want {
		got, name, typ := findLine(lines, w), lineName(w), printedType(w)
		switch {
		case got.text == "":
			return fmt.Errorf("no line of %s", name)
		case got.task != "default" || got.role != w.role || got.typ != typ || got.agent == "-":
			return fmt.Errorf("%s prints %q: want task default, role %s, type %s and an agent", name, got.text, w.role, typ)
		case got.started == "-" || !printedMoment(got.ended) || got.result != w.result:
			return fmt.Errorf("%s prints %q: want a start, an end and result %s", name, got.text, w.result)
		case w.kind == "step" && got.model != stepRecordsModel:
			return fmt.Errorf("%s prints %q: want the model %s", name, got.text, stepRecordsModel)
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

// lineName spells w as the command prints its leading fields, then its result: `4 step reviewer
// review attempt 1 … failed`.
func lineName(w stepWant) string {
	return fmt.Sprintf("%d %s %s %s attempt %d … %s", w.step, w.kind, w.role, printedType(w), w.attempt, w.result)
}

// The driver's end check, which the proof runs over what `dispatch steps` prints: a line ends its
// run only when it prints a moment as its end. steps.ts prints `running` there, and as the result,
// for a run that has not ended; `-`, which a printer that wrote the end through orDash would print,
// and any other word are no end either, so each fails the walk's read, naming the line. The control
// is the same ten lines with every end a moment, which the check passes.
func TestTheDriverReadsAnEndOnlyFromAMoment(t *testing.T) {
	t.Parallel()
	const key = "SR-1"
	printed := func(end, result string) string {
		lines := []string{key + ": 10 steps in 1 tasks"}
		for _, w := range stepRecordsTen {
			model, lineEnd, lineResult := "-", "2026-11-20T09:40:00.123456Z", w.result
			if w.kind == "step" {
				model = stepRecordsModel
			}
			if w.kind == "step" && w.step == 2 && w.attempt == 2 {
				lineEnd, lineResult = end, result
			}
			lines = append(lines, fmt.Sprintf("default %d %s %s %s attempt %d agent-%s %s 2026-11-20T09:00:00Z %s %s",
				w.step, w.kind, w.role, printedType(w), w.attempt, w.role, model, lineEnd, lineResult))
		}
		return strings.Join(lines, "\n") + "\n"
	}
	read := func(out string) error {
		parsed, err := parseSteps(key, out)
		if err != nil {
			t.Fatalf("parse %q: %v", out, err)
		}
		return holdsTen(parsed)
	}
	if err := read(printed("2026-11-20T09:40:00Z", "passed")); err != nil {
		t.Fatalf("control: the ten lines, each ended at a moment, read as %v, want the walk's ten", err)
	}
	for _, no := range []struct{ end, result string }{{"running", "running"}, {"-", "passed"}, {"unknown", "passed"}} {
		err := read(printed(no.end, no.result))
		if err == nil || !strings.Contains(err.Error(), "2 step implementer implementation attempt 2") || !strings.Contains(err.Error(), "an end") {
			t.Errorf("the ten lines with implementing's second run ending %q %q read as %v, want an error naming that run and its end", no.end, no.result, err)
		}
	}
}

// variantPlace reads `dispatch steps` as this release prints it. It holds the variant's line right
// after the header, and parseSteps reads the rest: the line of each task of the issue's plan,
// `task <task> needs <needs>: …`, stands before that task's runs and is no run, and the header's
// count is the runs below it. The fault is a read that takes the task line for a run, which fails
// the first row.
func TestVariantPlacePassesOverTheLineOfAPlansTask(t *testing.T) {
	const (
		run     = "default 1 step implementer implementation attempt 1 legion-leg-leg-1-implementer anthropic/claude-opus-5 2026-11-16T09:00:00Z 2026-11-16T09:40:00Z passed"
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
