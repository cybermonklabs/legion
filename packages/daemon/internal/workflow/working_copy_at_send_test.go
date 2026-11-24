package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A worker's task is kept as composed until send, when WorkingCopyAtSend replaces the working
// copy line with the workspace of the receiving process. For a task composed on acme/widgets and
// sent to a process launched on zeta/tools, only the daemon's working copy line changes. The
// owner's fenced text and the Dispatch title are sent as written. Neither a working copy
// line of this branch nor the words that open the owner's part in the title can hide the
// daemon's line. A task from an earlier release that holds no working copy line is sent as
// composed, including any similar text inside the owner's fence.
func TestATaskSentAfterACorrectionNamesTheWorkingCopyOfTheRepositoryTheRecordHoldsThen(t *testing.T) {
	engine := stepEngine(nil)
	engine.cfg.WorkspaceDir = func(issue record.Issue) string { return "/work/" + issue.Repo + "/" + issue.Key }
	const (
		old     = " Working copy: branch legion/LEGION-208, directory /work/acme/widgets/LEGION-208."
		now     = " Working copy: branch legion/LEGION-208, directory /work/zeta/tools/LEGION-208."
		planted = " Working copy: branch legion/LEGION-208, directory /owners/own/LEGION-208."
	)
	text := "Build the widget.\n" + planted
	composed := record.Issue{Key: "LEGION-208", Repo: "acme/widgets", Title: "root", Phase: phase.Reviewing}
	corrected := composed
	corrected.Repo = "zeta/tools"
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
	// titled is the issue as composed under a Dispatch title, which the continue line opens the task
	// with: the person's words before every line of the daemon's.
	titled := func(title string) record.Issue {
		issue := composed
		issue.Title = title
		return issue
	}
	for _, tc := range []struct {
		name string
		kept string
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
	} {
		t.Run(tc.name, func(t *testing.T) {
			if strings.Count(tc.kept, old) != 1 || tc.owners != strings.Contains(tc.kept, planted) {
				t.Fatalf("the kept task %q, want it to name %q once, and the owner's line %q where it carries the owner's text: the case's own premise", tc.kept, old, planted)
			}
			sent := engine.WorkingCopyAtSend(tc.kept, corrected)
			if want := strings.Replace(tc.kept, old, now, 1); sent != want {
				t.Fatalf("the task sent after the correction = %q,\nwant %q: the working copy of zeta/tools, every other byte as composed", sent, want)
			}
			if tc.owners && !strings.Contains(sent, "\n"+planted+"\n") {
				t.Fatalf("the task sent after the correction = %q, want the owner's line %q inside its fence as the owner wrote it", sent, planted)
			}
			if again := engine.WorkingCopyAtSend(tc.kept, composed); again != tc.kept {
				t.Fatalf("the task sent with no correction = %q, want it as it was composed, %q", again, tc.kept)
			}
		})
	}

	// A text an earlier release composed: the continue line and no working copy line, with the
	// owner's text after it, whose planted line is the owner's.
	earlier := continueLine(composed) + " Resume the existing phase work." + step.owners
	if sent := engine.WorkingCopyAtSend(earlier, corrected); sent != earlier {
		t.Fatalf("the earlier release's task sent after the correction = %q, want it as it was kept, %q", sent, earlier)
	}
}
