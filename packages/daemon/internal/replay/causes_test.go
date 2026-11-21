package replay

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// queueDropLines is the msg of each line the daemon writes where an issue's pull request leaves
// the merge queue (workflow's merge-queue handling): the one for a head the issue does not merge
// at, and the one for an issue in merging, whose merger is then told. A backward move out of
// merging after one of them is a drop from the merge queue that sends the work back.
var queueDropLines = []string{
	"workflow: a pull request left the merge queue where its issue does not merge at that head; nothing is read",
	"workflow: a pull request in merging left the merge queue; the daemon reads it from GitHub once",
}

// workflowDir is where package workflow's source stands beside this package's.
const workflowDir = "../workflow"

// wantCause is the cause the design gives a move back: a review's rejection, a failed test, a
// conflict and a red check by their triggers; a backward move by the phase it leaves, the
// architect's or the merger's send-back out of accepting, retro or merging and a worker's own
// before that; and a backward move out of merging after a drop from the merge queue, queue_drop.
func wantCause(move SendBack) Cause {
	switch move.Trigger {
	case workflow.TriggerReviewRejected:
		return CauseReviewChanges
	case workflow.TriggerTesterFailed:
		return CauseTestFailed
	case workflow.TriggerConflict:
		return CauseConflict
	case workflow.TriggerChecksRed:
		return CauseRedChecksOrConflict
	case workflow.TriggerBackward:
		switch {
		case move.Drop != "" && move.From == phase.Merging:
			return CauseQueueDrop
		case move.From == phase.Accepting || phase.MovesBack(move.From, phase.Accepting):
			return CauseSendBack
		default:
			return CauseOtherBackward
		}
	}
	return ""
}

// Every send-back loop is scored by its cause (spec R4), so every move back a recorded window can
// hold has one among the words of the design: every row of the tables a replay runs and of the
// recorded releases (recordedRows) whose To stands before its From in phase.Order, read on today's
// phases, and every backward move out of merging after each line of a pull request leaving the
// merge queue (queueDropLines, which must stand in package workflow's source, so a line renamed
// there fails here rather than never matching). It fails for a move CauseOf names no cause for, or
// one other than the design's (wantCause), and for a cause word outside Causes. It also holds the
// two groups a forecast across a recorded change scores the causes in, as the design decides them:
// the causes the change redefines and the ones both releases define the same way.
func TestEveryBackwardMoveHasACause(t *testing.T) {
	want := []Cause{"review_changes", "send_back", "conflict", "red_checks_or_conflict", "test_failed", "queue_drop", "other_backward"}
	if !slices.Equal(Causes, want) {
		t.Errorf("Causes is %q, want the design's words %q", Causes, want)
	}
	groups := map[ForecastGroup][]Cause{
		GroupRedefined: {CauseRedChecksOrConflict, CauseConflict, CauseSendBack},
		GroupSame:      {CauseReviewChanges, CauseTestFailed, CauseQueueDrop, CauseOtherBackward},
	}
	if !maps.EqualFunc(ForecastGroups, groups, slices.Equal[[]Cause]) {
		t.Errorf("ForecastGroups is %q, want %q: the causes a recorded change of the workflow redefines, scored only inside the total of all send-back loops, and the ones both releases define the same way, scored apart",
			ForecastGroups, groups)
	}
	grouped := make(map[Cause]ForecastGroup)
	for _, group := range slices.Sorted(maps.Keys(ForecastGroups)) {
		for _, cause := range ForecastGroups[group] {
			if other, twice := grouped[cause]; twice {
				t.Errorf("the cause %s is in the forecast groups %s and %s", cause, other, group)
			}
			grouped[cause] = group
			if !slices.Contains(Causes, cause) {
				t.Errorf("the forecast group %s holds %s, which is no word of Causes", group, cause)
			}
		}
	}
	for _, cause := range Causes {
		if _, ok := grouped[cause]; !ok {
			t.Errorf("the cause %s is in no forecast group: a forecast across a recorded change would score it nowhere", cause)
		}
	}

	// Two recorded rows can be one move on today's phases (a move out of awaiting_merge is one out
	// of merging), and each move is checked once.
	var moves []SendBack
	add := func(move SendBack) {
		if !slices.Contains(moves, move) {
			moves = append(moves, move)
		}
	}
	for _, row := range recordedRows(t) {
		from, to := fold(row.From), fold(row.To)
		if phase.MovesBack(from, to) {
			add(SendBack{Trigger: row.Trigger, From: from, To: to})
		}
	}
	source := workflowSource(t)
	for _, line := range queueDropLines {
		if !strings.Contains(source, fmt.Sprintf("%q", line)) {
			t.Errorf("package workflow's source writes no line %q: the drop from the merge queue the trace reads is gone or renamed", line)
		}
		for _, row := range recordedRows(t) {
			if row.Trigger == workflow.TriggerBackward && fold(row.From) == phase.Merging {
				add(SendBack{Trigger: row.Trigger, From: phase.Merging, To: fold(row.To), Drop: line})
			}
		}
	}
	if len(moves) == 0 {
		t.Fatal("no row of the tables moves back, so nothing was checked")
	}
	for _, move := range moves {
		cause, named := CauseOf(move)
		if !named {
			t.Errorf("the move back %s -> %s by %s (after the line %q) has no cause", move.From, move.To, move.Trigger, move.Drop)
			continue
		}
		if !slices.Contains(Causes, cause) {
			t.Errorf("the move back %s -> %s by %s has the cause %q, which is no word of Causes", move.From, move.To, move.Trigger, cause)
		}
		if want := wantCause(move); cause != want {
			t.Errorf("the move back %s -> %s by %s (after the line %q) has the cause %s, want %s", move.From, move.To, move.Trigger, move.Drop, cause, want)
		}
	}
}

// workflowSource is package workflow's own non-test source, every file of it as one text.
func workflowSource(t *testing.T) string {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(filepath.FromSlash(workflowDir), "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var source strings.Builder
	for _, path := range paths {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		source.Write(body)
	}
	if source.Len() == 0 {
		t.Fatalf("no source of package workflow under %s", workflowDir)
	}
	return source.String()
}
