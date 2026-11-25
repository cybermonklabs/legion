package admit

import (
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// Main's tree-stalled notice since cybermonklabs/legion#4622 (admit/stalled.go:stalledTreeReason
// at 74cb6855) ends "Read the tree and decide what it needs. You can ask a role what it waits on.
// You can park the tree.": it no longer tells the controller that it can move an issue back a
// phase, since no operation of the controller's `legion` tool makes that move
// (packages/daemon/.changes/unreleased/pr4622-tree-stalled-no-backward-move.yaml). The record
// names what the controller decides among in `decide`, so `decide` holds main's two choices, in
// main's order, and no move back.
func TestATreeStalledNoticeOffersNoMoveBackAPhase(t *testing.T) {
	members := []record.Issue{
		{Key: "LEGION-1240", Tree: "LEGION-1240", Phase: phase.Reviewing},
		{Key: "LEGION-1241", Tree: "LEGION-1240", Phase: phase.Testing},
	}
	notice := TreeStalledNotice("LEGION-1240", record.TreeQuiet{Quiet: 6 * time.Hour}, members, nil)
	want := []string{"ask_role_what_it_waits_on", "park_tree"}
	if !slices.Equal(notice.Decide, want) {
		t.Fatalf("the tree-stalled notice decides among %q, want %q: main's notice names asking a role what it waits on "+
			"and parking the tree, and no move of an issue back a phase, which no operation of the controller's makes", notice.Decide, want)
	}
}
