package admit

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/texts"
)

// Main's third arm (admit/architect_stall.go:architectSlowBeforeGate at d46ec4e0) counts from the
// first confirmation of the entry's task by a turn of the architect wherever that is later than
// the moment it would count from, with or without a pause: " That time is counted from %s, when a
// turn of its architect first confirmed a task of phase entry %d." The branch writes that as
// counted_from_event: entry-confirmed with no pause_id. INFRA-3686's R2: "a step that a text gives
// today goes into the reader's prompt where the prompt does not give it"; the change's own wrong
// fact is its defect (the architect's ruling). roles/controller-root.md explains counted_from only
// for "An `architect-stalled` notice whose count a pause of the project moved", so the notice of a
// root no pause touched, whose architect took its entry's task and then registered no gate step
// for 90 minutes, is told as a count a pause moved.
func TestASlowBeforeGateCountedFromAConfirmationWithNoPauseIsNotToldAsAPause(t *testing.T) {
	admitted := time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC)
	confirmed := admitted.Add(5 * time.Minute)
	now := confirmed.Add(2 * time.Hour)
	root := record.Issue{Key: "ALPHA-1", Tree: "ALPHA-1", Phase: phase.Admitted, Generation: 1}
	row := record.PhaseEntry{Entry: 4, RaisedAt: admitted}
	run := record.RoleRun{Claim: &record.RoleClaim{Handshake: supervise.EntryHandshake{Entry: 4, Generation: 1, QueuedAt: admitted, ConfirmedAt: confirmed}}}

	slow := architectSlowBeforeGate(root, record.TreeGate{}, admitted, row, run, stallFloor{}, time.Time{}, now)
	if slow == nil || slow.CountedFromEvent != texts.CountedFromEntryConfirmed || slow.PauseID != 0 {
		t.Fatalf("the arm read %+v, want a slow-before-gate notice counted from the entry's confirmation with no pause", slow)
	}

	body, err := os.ReadFile("../prompts/roles/controller-root.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.Contains(line, "`entry-confirmed` is") {
			continue
		}
		if strings.Contains(line, "whose count a pause of the project moved") {
			t.Errorf("controller-root.md explains counted_from_event: entry-confirmed only under %q, but this notice (no project pause, pause_id %d) counts from a confirmation: %s",
				"An `architect-stalled` notice whose count a pause of the project moved", slow.PauseID, line)
		}
		return
	}
	t.Fatal("controller-root.md has no line explaining `entry-confirmed`")
}
