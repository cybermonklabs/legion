package workflow

import (
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The design gate's note a task carries at its send (dispatch://INFRA-2040 R7, D15 of its plan)
// is a merger's in merging alone, and reads the one hold rule (HeldByDesignGate): the closed
// form while the gate holds the merger, the open form on a tree with a registered row of a gated
// step that holds nothing, and no note for another role, another phase, a tree whose gated steps
// have no row, or a deployment that gates no step. The closed form opens with the closing
// message's own words (gateClosedWords, which tellGateMoved writes) and with the form the merger
// prompt quotes (roles/merger.md, "A closed design gate"), so the merger reads the note as it
// reads that message.
func TestMergerGateNoteGivesAMergerInMergingTheGatesStateAndNobodyElseANote(t *testing.T) {
	const root = "LEGION-208"
	merging := record.Issue{Key: "LEGION-209", Tree: root, Project: "LEGION", Phase: phase.Merging, Generation: 3, Status: "in_progress"}
	inPhase := func(p phase.Phase) record.Issue {
		issue := merging
		issue.Phase = p
		return issue
	}
	approved := func(step record.DesignStep, document string, version int, seq int64) record.DesignGate {
		return record.DesignGate{Issue: root, Step: step, ArtifactID: document, LatestVersion: version, ApprovedVersion: &version,
			ApprovedMark: record.EventMark{Seq: seq}, EverOpened: true}
	}
	const (
		closed = "The design gate of LEGION-208 closed: version 2 of its spec document (spec-artifact) is with its reviewer. " +
			"Merge nothing, enqueue nothing, and take a pull request you queued back out of the merge queue."
		open = "The design gate of LEGION-208 stands open: every gated step that someone registered is approved."
	)

	for _, tc := range []struct {
		name  string
		issue record.Issue
		role  claim.Role
		gate  record.TreeGate
		want  supervise.GateNote
	}{
		{
			name: "the merger of an issue in merging, the spec it gates unapproved", issue: merging, role: claim.RoleMerger,
			gate: closedSpecGate(root), want: supervise.GateNote{Text: closed, State: "closed", Step: "spec", Version: 2},
		},
		{
			name: "the merger of an issue in merging, under an open gate", issue: merging, role: claim.RoleMerger,
			gate: openSpecGate(root), want: supervise.GateNote{Text: open, State: "open", Step: "spec", Version: 2},
		},
		{
			name: "the merger under an open gate of two steps, the first step's approval the newest", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec, record.DesignStepCriteria},
				Rows: map[record.DesignStep]record.DesignGate{
					record.DesignStepSpec:     approved(record.DesignStepSpec, "spec-artifact", 3, 9),
					record.DesignStepCriteria: approved(record.DesignStepCriteria, "criteria-artifact", 5, 4),
				}},
			want: supervise.GateNote{Text: open, State: "open", Step: "spec", Version: 3},
		},
		{
			name: "the merger under a gate whose one unapproved step has no row", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepCriteria, record.DesignStepSpec},
				Rows: map[record.DesignStep]record.DesignGate{record.DesignStepSpec: approved(record.DesignStepSpec, "spec-artifact", 2, 4)}},
			want: supervise.GateNote{Text: open, State: "open", Step: "spec", Version: 2},
		},
		{
			name: "the merger of a tree whose gated step has no row", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Steps: []record.DesignStep{record.DesignStepSpec}},
		},
		{
			name: "the merger under an empty gates.design over an unapproved spec row", issue: merging, role: claim.RoleMerger,
			gate: record.TreeGate{Rows: closedSpecGate(root).Rows},
		},
		{
			name: "the implementer of an issue in merging", issue: merging, role: claim.RoleImplementer,
			gate: closedSpecGate(root),
		},
		{
			name: "the merger of an issue in reviewing", issue: inPhase(phase.Reviewing), role: claim.RoleMerger,
			gate: closedSpecGate(root),
		},
		{
			name: "the merger of an issue in retro, under an open gate", issue: inPhase(phase.Retro), role: claim.RoleMerger,
			gate: openSpecGate(root),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := MergerGateNote(tc.issue, tc.role, tc.gate); got != tc.want {
				t.Fatalf("MergerGateNote = %+v, want %+v", got, tc.want)
			}
		})
	}

	note := MergerGateNote(merging, claim.RoleMerger, closedSpecGate(root))
	if words := gateClosedWords(root, record.DesignStepSpec, "spec-artifact", 2); !strings.HasPrefix(note.Text, words) {
		t.Errorf("the closed note %q does not open with the closing message's words %q", note.Text, words)
	}
	if quoted := "The design gate of " + root + " closed: version 2 of its spec document (spec-artifact)"; !strings.HasPrefix(note.Text, quoted) {
		t.Errorf("the closed note %q does not open with the form roles/merger.md quotes, %q", note.Text, quoted)
	}
}
