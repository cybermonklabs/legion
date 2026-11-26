package daemon

import (
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
