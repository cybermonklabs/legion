package daemon

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/runtime"
)

// The worker a declined release or a kept pod tells is the open phase's own worker, which can run
// beside a running fix, and the read that found the work counts the fix's working copy too
// (workspace.ReadWork). So no text that asks for a push names legion/<KEY> as the one branch: each
// says that a step beside a running fix pushes to its own branch beside that fix and never to
// legion/<KEY>, and that the fix's working copy is the fix's own to push. The red is the text
// before this rule, "Push the work that you keep to legion/<KEY>", which the role beside a fix
// would follow onto the task's branch.
func TestEveryKeptWorkTextNamesTheBranchEachWorkGoesTo(t *testing.T) {
	const issue = "LEGION-208"
	work := runtime.WorkspaceWork{Head: strings.Repeat("a", 40), Changed: true}
	for name, text := range map[string]string{
		"the role's declined release": declineMessage(issue, work),
		"the role's kept pod":         podKeptMessage(issue, "pod-1", "the image moved", work, false),
		"the role's kept pod again":   podKeptMessage(issue, "pod-1", "", work, true),
		"the architect's kept pod":    keptMessage(issue, "pod-1", nil, work, nil),
		"the architect's kept round":  roundKeptMessage(issue, "pod-1", "the image moved", nil, work, nil, false),
	} {
		for _, want := range []string{
			"never to legion/" + issue,
			"A step that runs beside a running fix pushes to its own branch beside that fix",
			"Work in the fix's working copy, beside the workspace, is the fix's",
		} {
			if !strings.Contains(text, want) {
				t.Errorf("%s does not say %q: %s", name, want, text)
			}
		}
		if strings.Contains(text, "Push the work that you keep to legion/"+issue) {
			t.Errorf("%s tells its worker to push all of the work to legion/%s: %s", name, issue, text)
		}
	}
}
