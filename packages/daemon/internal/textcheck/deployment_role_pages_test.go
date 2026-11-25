package textcheck

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/plan"
)

// Two texts an operator reads say what a role of the deployment takes. The deploy example says a
// hook's `roles` take any role of `roles` beside Legion's, as config.readHookRoles and
// checkDeploymentRoleKeys do. The deployment guide says an issue pod built before a role was
// defined runs it only once its update round or its release replaces the pod: until then a start
// the workflow asks for waits, and a launch the supervisor makes itself is refused and charged as a
// launch failure (runtime.ErrAwaitsPodUpdate). Each text is read as prose (proseOf), so a
// comment's layout decides nothing.
func TestTheOperatorTextsSayWhatARoleOfTheDeploymentTakes(t *testing.T) {
	for _, text := range []struct {
		path       string
		want, gone []string
	}{
		{
			path: "deploy/kubernetes/daemon/legion.yaml.example",
			want: []string{"roles are the roles it writes to, at least one, each implementer, tester, reviewer, planner, merger or a role of `roles`."},
			gone: []string{"at least one of implementer, tester, reviewer, planner and merger;"},
		},
		{
			path: "docs/deployment.md",
			want: []string{
				"An issue pod built before the role was defined runs it once the pod's update round or its release replaces the pod.",
				"Until then a start the workflow asks for waits, and a launch the supervisor makes itself, such as an operator's resume or a retry, is refused and charged as a launch failure",
			},
			gone: []string{"and until then the role's start waits."},
		},
	} {
		prose := proseOf(shippedText(t, text.path))
		for _, phrase := range text.want {
			if !strings.Contains(prose, phrase) {
				t.Errorf("%s omits %q", text.path, phrase)
			}
		}
		for _, phrase := range text.gone {
			if strings.Contains(prose, phrase) {
				t.Errorf("%s still says %q", text.path, phrase)
			}
		}
	}
}

// Three texts an operator reads count the moves back a hook may name, and each count is the
// loader's: config.readHookMoves takes a move from plan.MovesBackPairs alone, so the number is
// that set's size, the pairs of a step of a role the deployment defines among them. A pair of
// those is step-step, two equal phases, so no text says a move's two phases always differ.
// Each text is read as prose (proseOf), so a comment's layout decides nothing.
func TestTheOperatorTextsCountTheMovesBackAHookMayName(t *testing.T) {
	count := fmt.Sprintf("any other of the %d moves back a task's line can make", len(plan.MovesBackPairs()))
	for _, text := range []struct {
		path string
		want []string
	}{
		{path: "docs/site/src/content/docs/legion/concepts.md", want: []string{count}},
		{path: "docs/site/src/content/docs/legion/running-legion.md", want: []string{count, "each written `<from>-<to>` of two workflow phases, which differ but in `step-step`"}},
		{path: "deploy/kubernetes/daemon/legion.yaml.example", want: []string{count, "each written <from>-<to> of two workflow phases, which differ but in step-step"}},
	} {
		prose := proseOf(shippedText(t, text.path))
		for _, phrase := range text.want {
			if !strings.Contains(prose, phrase) {
				t.Errorf("%s omits %q", text.path, phrase)
			}
		}
		for _, gone := range []string{"of the 28 moves back", "two different workflow phases"} {
			if strings.Contains(prose, gone) {
				t.Errorf("%s still says %q", text.path, gone)
			}
		}
	}
}

// proseOf is body with each line's leading white space and `#` marks taken off (the example's
// comments stand in two columns, each opened by a `#`), and every run of white space made one
// space.
func proseOf(body string) string {
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		lines[i] = strings.TrimLeft(line, " \t#")
	}
	return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
}
