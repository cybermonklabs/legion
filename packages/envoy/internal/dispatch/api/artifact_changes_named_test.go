package api

import "testing"

// A version that differs from the one before it in more than its decision blocks names what
// differs: R2 of dispatch://INFRA-175 sends the session holding the work a task naming the changed
// sections, and a change set that names no section and no block tells it nothing of where to read.
// Moving a `##` section from one side of a top-level `#` heading to the other changes the
// document: the section now stands under the other heading, and neither the order of the `##`
// sections nor the text outside every section says so. The change set still names a section for
// it: the moved one, or the text outside every section, whose place among the sections changed.
// The control is the version before it, where the `#` heading is added: that change is named.
func TestAChangeSetNamesASectionMovedAcrossATopLevelHeading(t *testing.T) {
	t.Parallel()
	handler := newTestHandler(t)
	const (
		head     = "# Plan\n\nIntro.\n\n"
		goal     = "## Goal\n\nShip it.\n\n"
		scope    = "## Scope\n\nTwo regions.\n\n"
		risks    = "## Risks\n\nNone known.\n\n"
		appendix = "# Appendix\n\nBackground.\n\n"
	)
	artifactID := uploadDocumentVersions(t, handler, "plan.md",
		head+goal+scope+risks,
		head+goal+appendix+scope+risks,
		head+goal+scope+appendix+risks)

	added := readChanges(t, handler, artifactID, 1, 2)
	if added.DecisionOnly || len(added.Sections)+len(added.Blocks) == 0 {
		t.Fatalf("the version that adds the `# Appendix` heading: %+v, want it named", added)
	}
	moved := readChanges(t, handler, artifactID, 2, 3)
	if moved.DecisionOnly {
		t.Fatalf("decision_only = true for a version that moves Scope across `# Appendix`, want false: %+v", moved)
	}
	if len(moved.Sections)+len(moved.Blocks) == 0 {
		t.Fatalf("the version that moves Scope from below `# Appendix` to above it names no section and no block: %+v, want the moved section or the text outside every section named", moved)
	}
}
