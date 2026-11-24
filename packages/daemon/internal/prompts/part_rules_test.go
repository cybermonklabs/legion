package prompts

import (
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
)

// partText is one prompt part read alone, by its tree and name: "roles/<name>", a shared part
// (rolePart), or "go/<name>", one of this daemon's own. A test counts a rule's paragraphs here,
// part by part, because Compose joins a role's parts into one prompt: a rule written in two of
// them reads twice in the join, so a test of the join alone still passes when one copy is gone.
func partText(t *testing.T, part string) string {
	t.Helper()
	if name, ok := strings.CutPrefix(part, "go/"); ok {
		body, err := goParts.ReadFile("go/" + name)
		if err != nil {
			t.Fatalf("read go prompt part %s: %v", name, err)
		}
		return string(body)
	}
	return rolePart(t, strings.TrimPrefix(part, "roles/"))
}

// ruleIn holds part to one paragraph that says marker (ruleParagraphs over that part alone), and
// that paragraph to each of wants. A part that holds none fails, and so does one that holds two:
// the rule is written once in each part that carries it, since a second copy drifts from the first
// at the next edit. The wants are read in that paragraph, so the marker cannot stand in a paragraph
// that says something else.
func ruleIn(t *testing.T, part, marker string, wants ...string) {
	t.Helper()
	paragraphs := ruleParagraphs(partText(t, part), marker)
	if len(paragraphs) != 1 {
		t.Errorf("%s holds %d paragraphs that say %q, want one", part, len(paragraphs), marker)
		return
	}
	for _, want := range wants {
		if !strings.Contains(paragraphs[0], want) {
			t.Errorf("%s's paragraph that says %q omits %q: %s", part, marker, want, paragraphs[0])
		}
	}
}

// composedSays fails the test for each of wants the folded composed prompt of role does not say:
// the prompt the role is launched with carries the rule, whichever part holds it.
func composedSays(t *testing.T, composer *Composer, role claim.Role, isRoot bool, wants ...string) {
	t.Helper()
	text := foldWhitespace(composedText(t, composer, role, isRoot))
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("the composed %s prompt (root %t) omits %q", role, isRoot, want)
		}
	}
}

// An approved new version of a tree's design reaches the session holding each affected issue's
// phase as a news task (workflow.RenderNews), and the daemon judges nothing of what it means for
// the work: the holder decides (dispatch://INFRA-175 R2 to R5, Design 7). So every phase worker's
// composed prompt carries the two answers, written once in the part every phase worker shares: a
// move back with a reason naming the version and the change when the work is stale, with the
// implementer alone pushing first, or a comment naming the version when it is current. The planner
// carries its own answer, since a stale plan is its own work wherever its step stands. The merger carries
// the opening a news task can bring in place of the opened message, and the WAITING line that
// lets a task reach it between turns. Every architect carries the answers in `accepting` (a
// sign-off, or a move back to planning, implementing or testing), the root architect's summary
// duty for a member whose pull request merged, and R4's message: the architect's own Envoy message
// for a new version the daemon tells nobody of. Each rule is counted once in each part that holds
// it, then read in the composed prompt of each role, and the architect parts no longer say the
// moves back that left planning out. This shows the text is loaded, never that a role follows it.
func TestTheComposedPromptsAnswerAnApprovedNewVersion(t *testing.T) {
	composer, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Run("phase workers", func(t *testing.T) {
		const news = "`The approved design of <tree> changed for <issue>, which you hold in <phase>:`"
		answers := []string{
			news,
			"If your work is stale, call the `legion` tool's `request_backward_move` with `to`",
			"a `reason` that names the version and the change",
			"Before that call, the implementer pushes the work it has. No other role pushes before it.",
			"If your work is current, write a `dispatch comment` on the issue that names the version",
			"An issue whose pull request has merged never moves back",
		}
		ruleIn(t, "go/worker-common.md", news, answers...)
		for _, role := range []claim.Role{claim.RolePlanner, claim.RoleImplementer, claim.RoleTester, claim.RoleReviewer, claim.RoleMerger} {
			composedSays(t, composer, role, false, answers...)
		}
	})
	t.Run("planner", func(t *testing.T) {
		const revise = "Revise `plan.md` in place to match the new version of the design"
		ruleIn(t, "go/planner.md", revise, "news task", "A stale plan is your own work wherever your task places your step", "run the plan review again")
		if strings.Contains(goPart(t, "planner.md"), "Planning has no earlier phase") {
			t.Error("go/planner.md still says planning has no earlier phase, false for a planner after the implementer")
		}
		composedSays(t, composer, claim.RolePlanner, false, revise)
	})
	t.Run("merger", func(t *testing.T) {
		const (
			opening = "The opening can also come as a news task"
			waiting = "sends a task only between turns"
		)
		for _, part := range []string{"roles/merger.md", "go/merger.md"} {
			ruleIn(t, part, opening, "before you merge", "`request_backward_move`")
			ruleIn(t, part, waiting, "each turn", "`WAITING`")
		}
		composedSays(t, composer, claim.RoleMerger, false, opening,
			"`The design gate of <tree> opened: a person approved version <n> of its <step> document (<id>), and every step the deployment gates now stands open`",
			"end each turn with a line that starts `WAITING`", "The daemon sends a task only between turns")
	})
	t.Run("architects", func(t *testing.T) {
		const (
			accepting = "`The approved design of <tree> changed for <issue>, which you hold in accepting:`"
			summary   = "`A person approved a new version of the design of <tree>.`"
			message   = "Request no approval for it."
		)
		// The moves back name the phase of an earlier step of the issue's task, any of them, as the
		// owner's plan reads them (TestAMoveBackGoesToAnyEarlierStepInTheGoParts): a list of named
		// phases holds only for the default steps, since a reviewer step before the acceptance is
		// an earlier step too.
		answers := []string{accepting, "to the phase of an earlier step of its task, with a `reason` that names the version and the change"}
		duty := []string{summary, "a merged change never moves back", "whose pull request is still open", "`dispatch ask` the person on the root"}
		r4 := []string{message, "`envoy_publish` to the role topic of the phase holder of each issue the version changes, naming the version and what changed"}
		ruleIn(t, "go/architect-common.md", accepting, answers...)
		ruleIn(t, "go/architect-common.md", summary, duty...)
		ruleIn(t, "go/architect-common.md", message, append(slices.Clone(r4), "Under a `Design gate steps` line that reads `none`")...)
		ruleIn(t, "roles/architect.md", accepting, answers...)
		ruleIn(t, "roles/architect.md", message, r4...)
		// The moves back from accepting as the architect parts named them before: each named a list
		// of phases an architect may send an issue back to, which left planning out, or, with
		// planning, left out every other earlier step of the task. The skill's own sentence is pinned
		// in pi-legion's skills-guard.test.ts, since no composed prompt reads the skill.
		retired := []string{"(`implementing` or `testing`)", "`to` `implementing` or `testing`", "back to `implementing` or `testing`",
			"`implementing`, or to `planning` or `testing` where its task has that step"}
		for _, isRoot := range []bool{true, false} {
			composedSays(t, composer, claim.RoleArchitect, isRoot, slices.Concat(answers, duty, r4)...)
			text := foldWhitespace(composedText(t, composer, claim.RoleArchitect, isRoot))
			for _, phrase := range retired {
				if strings.Contains(text, phrase) {
					t.Errorf("the composed architect prompt (root %t) still says %q", isRoot, phrase)
				}
			}
		}
	})
}
