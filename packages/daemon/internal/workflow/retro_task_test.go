package workflow

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// stepsSentence finds a task's sentence that lists its steps (stepsLine), which every task of a
// run with plan rows carries: the task text tests read it off the task of an earlier phase of the
// same run, so a retro's task is held to the line the code writes for that run, not to a copy of
// its words.
var stepsSentence = regexp.MustCompile(" " + regexp.QuoteMeta(stepsLabel) + `: [^.]*\.`)

// architectTopic is the role topic of issue's architect, which an agent publishes a finding to:
// the Envoy role topic prefix and the architect's claim token, as the daemon spells a role's
// topic everywhere it publishes one.
func architectTopic(t *testing.T, issue string) string {
	t.Helper()
	return notify.RoleTopicPrefix + treeRoleToken(t, issue, claim.RoleArchitect)
}

// taskTree runs the tree these tests read a retro task in: the root LEGION-208, its child
// LEGION-209 in implementing, and the grandchild LEGION-210 under LEGION-209, with its pull request
// numbered pr (none for 0), started and in implementing. LEGION-209's architect owns the grandchild,
// and LEGION-208's is the tree's root architect, so the two topics a task could name differ. It
// answers the steps sentence of the grandchild's implementer task.
func taskTree(t *testing.T, r *retroRig, pr int) string {
	t.Helper()
	r.root(t, phase.Implementing)
	r.member(t, "LEGION-209", "LEGION-208", phase.Admitted, 43)
	r.runTo(t, "LEGION-209", phase.Implementing)
	r.member(t, "LEGION-210", "LEGION-209", phase.Admitted, pr)
	r.runTo(t, "LEGION-210", phase.Implementing)
	implementing := phaseStartsOf(superviseRowsOf(t, r.pool, "LEGION-210", "start"), phase.Implementing)
	if len(implementing) != 1 {
		t.Fatalf("starts of the grandchild's implementer = %+v, want the one of its implementing", implementing)
	}
	steps := stepsSentence.FindString(implementing[0].Task)
	if steps == "" {
		t.Fatalf("the grandchild's implementer task names no steps: %q", implementing[0].Task)
	}
	return steps
}

// The task of a retro after a close (P11) opens as every retro task does, carries the task's steps
// as every task of its run does, names the pull request only where the issue has one, and sends a
// finding to the tree's root architect: it names the role topic of LEGION-208's architect, the
// root's, and never that of LEGION-209's, which owns the grandchild that closed. The start that
// carries it names no phase.
func TestTheRetroTaskAfterACloseNamesTheRootArchitect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		pr   int
	}{
		{name: "with its pull request", pr: 44},
		{name: "with no pull request"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			steps := taskTree(t, r, tc.pr)
			mark := r.mark(t)
			r.person(t, "LEGION-210", "done")

			start := assertOneRetroStart(t, r.pool, "LEGION-210", 3, mark)
			if root := architectTopic(t, "LEGION-208"); !strings.Contains(start.Task, root) {
				t.Fatalf("the retro task after the close names no %s, the root architect's topic:\n%s", root, start.Task)
			}
			if owner := architectTopic(t, "LEGION-209"); strings.Contains(start.Task, owner) {
				t.Fatalf("the retro task after the close names %s, the owning architect's topic, want the root's alone:\n%s", owner, start.Task)
			}
			if !strings.Contains(start.Task, steps) {
				t.Fatalf("the retro task after the close lacks the run's steps %q:\n%s", steps, start.Task)
			}
			named := fmt.Sprintf(" %s: #%d.", pullRequestLabel, tc.pr)
			if tc.pr != 0 && !strings.Contains(start.Task, named) {
				t.Fatalf("the retro task after the close lacks %q:\n%s", named, start.Task)
			}
			if tc.pr == 0 && strings.Contains(start.Task, " "+pullRequestLabel+":") {
				t.Fatalf("the retro task of an issue with no pull request names one:\n%s", start.Task)
			}
		})
	}
}

// The retro task of an issue still in the workflow whose pull request merged before its retro
// sends a finding to the tree's root architect too (R2): it names the root architect's role topic
// and never the owning architect's, beside the retro's first line, the run's steps and the pull
// request. The control is the same issue whose pull request is open: its retro is the ordinary
// one, and its task names no architect's topic.
func TestTheRetroTaskOfAnOpenIssueWhosePullRequestMergedNamesTheRootArchitect(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		merged bool
	}{
		{name: "its pull request merged", merged: true},
		{name: "its pull request open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRetroRig(t)
			steps := taskTree(t, r, 44)
			if tc.merged {
				r.apply("merged-44", intake.PullRequestMerged{Repo: "sjawhar/legion", Number: 44, MergeSHA: "merge-44"})
				issueStandsIn(t, r.pool, "LEGION-210", phase.Implementing)
			}
			r.walkTo(t, "LEGION-210", phase.Retro)

			retro := phaseStartsOf(superviseRowsOf(t, r.pool, "LEGION-210", "start"), phase.Retro)
			if len(retro) != 1 {
				t.Fatalf("starts of the grandchild's retro = %+v, want one", retro)
			}
			task := retro[0].Task
			at := *issueRecord(t, r.pool, "LEGION-210")
			if lead := taskOpening(at, WorkingCopy(at, r.engine.cfg.WorkspaceDir)); !strings.HasPrefix(task, lead) {
				t.Fatalf("the retro task opens %q, want %q", task, lead)
			}
			if !strings.Contains(task, steps) {
				t.Fatalf("the retro task lacks the run's steps %q:\n%s", steps, task)
			}
			if named := fmt.Sprintf(" %s: #44.", pullRequestLabel); !strings.Contains(task, named) {
				t.Fatalf("the retro task lacks %q:\n%s", named, task)
			}
			if owner := architectTopic(t, "LEGION-209"); strings.Contains(task, owner) {
				t.Fatalf("the retro task names %s, the owning architect's topic:\n%s", owner, task)
			}
			root := architectTopic(t, "LEGION-208")
			if tc.merged && !strings.Contains(task, root) {
				t.Fatalf("the retro task of an open issue whose pull request merged names no %s, the root architect's topic:\n%s", root, task)
			}
			if !tc.merged && strings.Contains(task, root) {
				t.Fatalf("the retro task of an open issue whose pull request is open names %s, want the ordinary retro task:\n%s", root, task)
			}
		})
	}
}
