package prompts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoFile is one file of the checkout by its path from the repository's root, which is four
// levels above this package.
func repoFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("..", "..", "..", "..", path))
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(body)
}

// saysAndRetires holds each text of the checkout (repoFile) to the words it must say and the words
// it must no longer say, over a body whose whitespace is folded, reading and folding each text once.
func saysAndRetires(t *testing.T, says, retired [][2]string) {
	t.Helper()
	bodies := map[string]string{}
	body := func(path string) string {
		t.Helper()
		folded, ok := bodies[path]
		if !ok {
			folded = foldWhitespace(repoFile(t, path))
			bodies[path] = folded
		}
		return folded
	}
	for _, pin := range says {
		if !strings.Contains(body(pin[0]), pin[1]) {
			t.Errorf("%s does not say %q", pin[0], pin[1])
		}
	}
	for _, pin := range retired {
		if strings.Contains(body(pin[0]), pin[1]) {
			t.Errorf("%s still says %q", pin[0], pin[1])
		}
	}
}

// The guides say what the daemon does: an issue with no pull request closes at its retro's end
// (workflow's table, noPullRequest), the fresh agent's refused pass is taken when sent again
// (handoff leaves it unspent), an approval ends a round once GitHub reports the head mergeable
// (review.go reviewRound), and an issue's agents share one pod.
func TestTheGuidesSayHowAnIssueWithNoPullRequestClosesAndWhatEndsAReviewRound(t *testing.T) {
	saysAndRetires(t, [][2]string{
		{"docs/site/src/content/docs/legion/concepts.md", "Another agent's refused pass is taken when it sends it again once they have left."},
		{"docs/site/src/content/docs/legion/concepts.md", "as one comment on the issue where a point qualifies"},
		{"docs/site/src/content/docs/legion/concepts.md", "ends the round whatever the CI verdict at that commit, once GitHub reports that head one the issue may advance on"},
		{"docs/site/src/content/docs/legion/concepts.md", "an issue with no pull request, such as one whose only step is `tester`, is done when its retro ends"},
		{"docs/site/src/content/docs/legion/index.md", "An issue with no pull request closes when its retro ends, with no merge."},
		{"docs/site/src/content/docs/legion/index.md", "Phase workers, in the issue's one pod"},
		{"docs/site/src/content/docs/legion/using-legion.md", "An issue with no pull request has no merge: it moves to `done` when its retro ends."},
		{"docs/site/src/content/docs/index.mdx", "and set the branch rules each merge must meet."},
		{"README.md", "An issue with no pull request is Done when its retro ends"},
	}, [][2]string{
		{"docs/site/src/content/docs/legion/concepts.md", "applies by itself once they have left"},
		{"docs/site/src/content/docs/legion/concepts.md", "whatever CI says at that commit"},
		{"docs/site/src/content/docs/legion/index.md", "one Kubernetes pod each"},
		{"docs/site/src/content/docs/index.mdx", "and merge the result"},
		{"README.md", "An issue whose steps open no pull request"},
		{"docs/site/src/content/docs/legion/using-legion.md", "An issue whose steps opened no pull request"},
	})
}

// The owner's sign_off and request_backward_move keep the pull request's head, and complete_step
// the review record's (workflow's recordOwnersReview, recordStepReview); owner-does-step.sh proves no
// step acceptance, and its driver posts the route with the commit itself.
func TestTheOperatorGuidesSayWhichHeadTheOwnersReviewKeepsAndWhatTheDriversProve(t *testing.T) {
	saysAndRetires(t, [][2]string{
		{"docs/kubernetes.md", "Where the owner's `sign_off` ended its `acceptance:owner`, or its `request_backward_move` ended any review step of its own"},
		{"docs/kubernetes.md", "The owner's `complete_step` at `reviewer:owner` holds the head of the `review` record it reported instead"},
		{"docs/kubernetes.md", "At a review step of a role the deployment or the plan defines, it holds the commit the owner's workspace stands on."},
		{"docs/kubernetes.md", "At `acceptance:owner` it is applied as `sign_off` or `request_backward_move`, so it holds the pull request's head."},
		{"scripts/e2e/README.md", "A step `acceptance` with no key is a fresh agent of the role `acceptance`."},
		{"scripts/e2e/README.md", "which the driver posts itself (an agent's `legion` tool fills the commit in)"},
	}, [][2]string{
		{"scripts/e2e/README.md", "a step `acceptance` with no key is a fresh agent, and a task with no pull request is done"},
	})
}

// The guides say that the owner's retro of a task whose last implementation step is its own acts
// as that step's App, the implement App, which alone resolves the pull request's review threads
// (record.OwnerActsAs, record.RetroActsAs), and that the owner's own retro of a task with no such
// step stays on the owner's App.
func TestTheGuidesSayTheOwnersRetroOfItsImplementationStepActsAsTheImplementApp(t *testing.T) {
	saysAndRetires(t, [][2]string{
		{"docs/site/src/content/docs/legion/running-legion.md", "The retro of a task whose last implementation step is the architect's own acts as the implement App, as that step did."},
		{"docs/site/src/content/docs/legion/concepts.md", "The architect acts as that role where the step was its own, and keeps its own App where the task has none"},
	}, [][2]string{
		{"docs/site/src/content/docs/legion/running-legion.md", "(its acceptance, a tester or reviewer step, its retro), needs no write"},
	})
}
