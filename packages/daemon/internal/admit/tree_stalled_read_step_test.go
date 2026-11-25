package admit

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's tree-stalled notice (admit/stalled.go:stalledTreeReason at d46ec4e0 and c03d3afe) ends:
// "Read the tree and decide what it needs. You can ask a role what it waits on. You can move an
// issue back a phase. You can park the tree." Main's #4622 (dispatch://INFRA-4673) took out 'You can
// move an issue back a phase.', because no operation of the controller's legion tool makes that move,
// so R8 keeps R5's read step and its other two choices. dispatch://INFRA-2560's R5 (approved, done) has the
// notice give that step: "It tells the controller to read the tree and decide: ask a role what it
// waits on, move an issue back a phase, or park the tree." dispatch://INFRA-3686's R8 keeps it: "a
// fact or a step that it has the text give stays as a field". The branch's record names the three
// choices in `decide` (texts.TreeStalledChoices), and its doc comment says R8 "keeps that step as a
// field", but no field says to read the tree: that part of R5's step is only in
// prompts/roles/controller-root.md.
func TestATreeStalledNoticeTellsTheControllerToReadTheTreeAsAField(t *testing.T) {
	members := []record.Issue{
		{Key: "LEGION-1230", Tree: "LEGION-1230", Phase: phase.Reviewing},
		{Key: "LEGION-1231", Tree: "LEGION-1230", Phase: phase.Testing},
	}
	notice := TreeStalledNotice("LEGION-1230", record.TreeQuiet{Quiet: 6 * time.Hour}, members, nil)
	if !slices.Equal(notice.Decide, []string{texts.DecideAskRole, texts.DecidePark}) {
		t.Fatalf("the tree-stalled notice decides among %q; want the two choices main keeps since #4622: ask a role what it waits on, park the tree", notice.Decide)
	}
	encoded, err := json.Marshal(notice)
	if err != nil {
		t.Fatalf("encode the tree-stalled notice: %v", err)
	}
	var payload any
	if err := json.Unmarshal(encoded, &payload); err != nil {
		t.Fatalf("decode %s: %v", encoded, err)
	}
	for _, value := range readStepValues(payload) {
		if strings.Contains(value, "read") && strings.Contains(value, "tree") {
			return
		}
	}
	t.Errorf("the tree-stalled notice is %s: its fields name the choices R5 gives (`decide`) and nothing says to read the tree first, "+
		"which R5 has the notice tell the controller (\"read the tree and decide\") and main's notice said (\"Read the tree and decide what it needs.\")",
		encoded)
}

// readStepValues is every string a decoded JSON payload holds, in its values and in the elements of
// its lists, at any depth.
func readStepValues(payload any) []string {
	switch value := payload.(type) {
	case string:
		return []string{value}
	case []any:
		var all []string
		for _, element := range value {
			all = append(all, readStepValues(element)...)
		}
		return all
	case map[string]any:
		var all []string
		for _, element := range value {
			all = append(all, readStepValues(element)...)
		}
		return all
	}
	return nil
}
