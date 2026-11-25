package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/ghrepo"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// earlierOpening is the continue line a task an earlier release composed opens with, for the issue
// LEGION-208 titled root in reviewing, and the space after it, where that release put the working
// copy line.
const earlierOpening = "Continue root. Issue: LEGION-208. Phase: reviewing. "

// A worker's task is kept as composed until send, when WorkingCopyAtSend replaces the working
// copy line with the workspace of the receiving process. For a task composed on acme/widgets and
// sent to a process launched on zeta/tools, only the daemon's working copy line changes. The
// owner's fenced text and the Dispatch title are sent as written. The line opens the task, before
// the continue line and the title in it, so nothing a person wrote stands before it: a title that
// holds a working copy line of this branch, the words that open the owner's part, the continue
// line's own ending, or that ending and a whole working copy line naming the composed directory,
// neither hides the daemon's line nor is rewritten in its place. A task an earlier release
// composed, which holds the line right after its continue line, is written again too. A task from
// an earlier release that holds no working copy line is sent as composed, including any similar
// text inside the owner's fence.
func TestATaskSentToAProcessOfAnotherRepositoryNamesThatProcessesWorkingCopy(t *testing.T) {
	engine := stepEngine(nil)
	engine.cfg.WorkspaceDir = func(key string, repo ghrepo.Repository) string { return "/work/" + repo.String() + "/" + key }
	const (
		old     = "Working copy: branch legion/LEGION-208, directory /work/acme/widgets/LEGION-208."
		now     = "Working copy: branch legion/LEGION-208, directory /work/zeta/tools/LEGION-208."
		planted = " Working copy: branch legion/LEGION-208, directory /owners/own/LEGION-208."
	)
	text := "Build the widget.\n" + planted
	composed := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", Title: "root", Phase: phase.Reviewing}
	corrected := ghrepo.MustParse("zeta/tools")
	steps := []record.PlanStep{{Task: "a", Position: 1, Role: "implementer"}, {Task: "a", Position: 2, Role: "reviewer"}}
	stepWalk := Walk{Stored: true, Task: "a", Text: text, Steps: steps, Line: []phase.Phase{phase.Implementing, phase.Reviewing, phase.Retro, phase.Merging}}
	rows := []record.PhaseRow{{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1}}
	handoff := record.PhaseRow{Role: claim.RoleImplementer, HandoffRecord: "implement-1", HandoffVersion: 1, HandoffCommit: "head"}
	reviewer := record.PhaseRow{Role: claim.RoleReviewer, Entry: 3}
	standing := func(w Walk) Standing {
		at, err := standingText(composed, engine.cfg.WorkspaceDir, w, rows)
		if err != nil {
			t.Fatal(err)
		}
		return at
	}
	step, today := standing(stepWalk), standing(todaysWalk())
	// titled is the issue as composed under a Dispatch title, which the continue line holds: the
	// person's words, after the daemon's working copy line.
	titled := func(title string) record.Issue {
		issue := composed
		issue.Title = title
		return issue
	}
	for _, tc := range []struct {
		name string
		kept string
		// at is where the daemon's working copy line stands in the kept task: 0, its start, in this
		// release's shape.
		at int
		// owners is whether the kept text holds the owner's text with its planted line.
		owners bool
	}{
		{name: "a start of an issue with no plan rows", kept: task(composed, today, handoff, &record.PullRequest{Number: 42}, "")},
		{name: "a resume of an issue with no plan rows", kept: resumeText(composed, today)},
		{name: "a start's task that carries news after it", kept: task(composed, today, handoff, nil, "") + "\n\nThe approved spec moved from version 4 to 5."},
		{name: "a step's start", kept: namingTheReviewRound(task(composed, step, handoff, nil, "the reviewer asked again"),
			claim.RoleReviewer, phase.Reviewing, reviewer), owners: true},
		{name: "a step's resume", kept: resumeText(composed, step), owners: true},
		{name: "a rework whose title holds a working copy line of the issue's branch", kept: task(titled("Name the"+
			" Working copy: branch legion/LEGION-208, directory /work/elsewhere/LEGION-208."+" in the prompt"), today, record.PhaseRow{}, nil, "the reviewer asked again")},
		{name: "a step's resume whose title holds the opening of the owner's part", kept: resumeText(titled("Quote"+ownersOpening+" in the prompt"), step), owners: true},
		{name: "a start whose title holds the continue line's own ending", kept: task(titled("Notes. Issue: LEGION-208. Phase: draft"),
			today, handoff, &record.PullRequest{Number: 42}, "")},
		{name: "a step's resume whose title holds the continue line's ending and a whole working copy line", kept: resumeText(titled(
			"Notes. Issue: LEGION-208. Phase: draft. Working copy: branch legion/LEGION-208, directory /work/acme/widgets/LEGION-208."), step), owners: true},
		{name: "a task an earlier release composed, its line right after its continue line",
			kept: earlierOpening + old + " Resume the existing phase work.", at: len(earlierOpening)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.HasPrefix(tc.kept[tc.at:], old) || tc.owners != strings.Contains(tc.kept, planted) {
				t.Fatalf("the kept task %q, want it to name %q at byte %d, and the owner's line %q where it carries the owner's text: the case's own premise", tc.kept, old, tc.at, planted)
			}
			sent := engine.WorkingCopyAtSend(tc.kept, composed.Key, corrected)
			if want := tc.kept[:tc.at] + now + tc.kept[tc.at+len(old):]; sent != want {
				t.Fatalf("the task sent after the correction = %q,\nwant %q: the working copy of zeta/tools in the daemon's line, every other byte as composed, the title too", sent, want)
			}
			if tc.owners && !strings.Contains(sent, "\n"+planted+"\n") {
				t.Fatalf("the task sent after the correction = %q, want the owner's line %q inside its fence as the owner wrote it", sent, planted)
			}
			if again := engine.WorkingCopyAtSend(tc.kept, composed.Key, ghrepo.MustParse(composed.Repo)); again != tc.kept {
				t.Fatalf("the task sent with no correction = %q, want it as it was composed, %q", again, tc.kept)
			}
		})
	}

	// A text an earlier release composed: the continue line and no working copy line, with the
	// owner's text after it, whose planted line is the owner's.
	earlier := continueLine(composed) + " Resume the existing phase work." + step.owners
	if sent := engine.WorkingCopyAtSend(earlier, composed.Key, corrected); sent != earlier {
		t.Fatalf("the earlier release's task sent after the correction = %q, want it as it was kept, %q", sent, earlier)
	}
}
