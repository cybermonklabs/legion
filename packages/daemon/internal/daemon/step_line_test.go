package daemon

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// runFields is the 16 words of text where text is one run as `dispatch steps` prints it. It
// answers false where text is no run. A line of 12 words, as the command printed before the set
// model, is no run. The e2e drivers parse each run by this rule.
func runFields(text string) ([]string, bool) {
	fields := strings.Fields(text)
	return fields, len(fields) == 16 && fields[5] == "attempt" && fields[12] == "set" && fields[14] == "answered"
}

func TestRunFieldsReadsTheWordsDispatchStepsPrints(t *testing.T) {
	t.Parallel()
	const run = "default 1 step planner planning attempt 1 agent-planner stand-in/model 2026-11-20T09:00:00Z running running set - answered running"
	for _, tc := range []struct {
		name string
		text string
		ok   bool
	}{
		{"the current line", run, true},
		{"the line before the set model", strings.Join(strings.Fields(run)[:12], " "), false},
		{"a different attempt word", strings.Replace(run, " attempt ", " tries ", 1), false},
		{"a different set word", strings.Replace(run, " set ", " with ", 1), false},
		{"a different answered word", strings.Replace(run, " answered ", " counts ", 1), false},
	} {
		fields, ok := runFields(tc.text)
		if ok != tc.ok {
			t.Errorf("%s: runFields(%q) = %v with %d words, want %v", tc.name, tc.text, ok, len(fields), tc.ok)
		}
	}
}

// variantPlace answers nil where line stands in its place in printed, the stdout of `dispatch
// steps --issue key`. That place is right after the header `<key>: <n> steps in <m> tasks`. The
// header's n runs follow it (runFields), and nothing else. It answers where the line stands
// otherwise.
func variantPlace(key, line, printed string) error {
	lines := strings.Split(strings.TrimRight(printed, "\n"), "\n")
	header, ok := strings.CutPrefix(lines[0], key+": ")
	var steps, tasks int
	if _, err := fmt.Sscanf(header, "%d steps in %d tasks", &steps, &tasks); !ok || err != nil {
		return fmt.Errorf("the first line %q is no header of %s", lines[0], key)
	}
	if at := slices.Index(lines, line); at != 1 {
		if at < 1 {
			return fmt.Errorf("no line below the header is %q", line)
		}
		return fmt.Errorf("it is line %d of %d, after %q; want it right after the header and before the first run", at+1, len(lines), lines[at-1])
	}
	runs := lines[2:]
	for _, run := range runs {
		if _, ok := runFields(run); !ok {
			return fmt.Errorf("the line %q below it is no run", run)
		}
	}
	if steps == 0 || len(runs) != steps {
		return fmt.Errorf("the header says %d steps and %d runs follow the line", steps, len(runs))
	}
	return nil
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
	if err := variantPlace("SR-1", line, withLine(old)); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("the line %q below it is no run", first)) {
		t.Errorf("the ten runs in the 12 words of the command before the set model read as %v, want an error naming %q", err, first)
	}
}
