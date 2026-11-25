package workflow

import (
	"reflect"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The answer to the news of an issue in accepting names the phases a move back reaches: the phases
// of the steps of its task before the acceptance, and no other (MoveBackTargets). The default
// steps name the four phases before accepting, reviewing among them; `implementer reviewer
// acceptance:owner` names implementing and reviewing, and neither planning nor testing, which that
// task has no step for. A line with no step before an acceptance, or with no acceptance, is an
// error, so no answer names no move back.
func TestMoveBackTargetsAreThePhasesOfTheStepsBeforeTheAcceptance(t *testing.T) {
	line := func(steps ...phase.Phase) []phase.Phase { return append(steps, phase.Retro, phase.Merging) }
	for _, tc := range []struct {
		name string
		line []phase.Phase
		want []phase.Phase
	}{
		{name: "the default steps", line: phase.Order, want: []phase.Phase{phase.Planning, phase.Implementing, phase.Testing, phase.Reviewing}},
		{name: "implementer reviewer acceptance:owner", line: line(phase.Implementing, phase.Reviewing, phase.Accepting),
			want: []phase.Phase{phase.Implementing, phase.Reviewing}},
		{name: "implementer acceptance:owner tester", line: line(phase.Implementing, phase.Accepting, phase.Testing),
			want: []phase.Phase{phase.Implementing}},
		{name: "a line with no acceptance", line: line(phase.Implementing, phase.Reviewing)},
		{name: "an acceptance with no step before it", line: line(phase.Accepting, phase.Implementing)},
		{name: "no line"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MoveBackTargets(tc.line)
			if tc.want == nil {
				if err == nil {
					t.Fatalf("MoveBackTargets(%v) = %v, want an error", tc.line, got)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("MoveBackTargets(%v) = %v, %v; want %v", tc.line, got, err, tc.want)
			}
		})
	}
}
