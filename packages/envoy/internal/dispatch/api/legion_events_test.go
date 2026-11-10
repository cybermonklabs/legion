package api

import (
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"
)

// legionPush posts one delivery of change events and answers last_seq.
func legionPush(t *testing.T, handler http.Handler, body map[string]any) int64 {
	t.Helper()
	response := dispatchRequest(t, handler, http.MethodPost, "/api/v1/legion/events", body, "alice")
	if response.Code != http.StatusOK {
		t.Fatalf("POST the Legion events: status=%d body=%s", response.Code, response.Body.String())
	}
	return decodeBody[struct {
		LastSeq int64 `json:"last_seq"`
	}](t, response).LastSeq
}

// TestASnapshotKeepsEveryIssuesPhaseSlotAndRoles: a daemon's boot snapshot, a repair snapshot
// after it and a heartbeat each leave every running issue on the board with its phase, its slot
// and the roles in a turn (dispatch://INFRA-979: the box showed neither slots nor roles).
func TestASnapshotKeepsEveryIssuesPhaseSlotAndRoles(t *testing.T) {
	handler, database := newTestHandlerWithStore(t)
	seedLegionProjects(t, handler, "INFRA")
	nameLegionDaemon(t, handler, "INFRA", "http://legion-infra.internal:13370")
	type running struct {
		phase, role string
		slot        int
	}
	issues := map[string]running{}
	for number, phase := range []string{"planning", "implementing", "testing", "reviewing", "accepting", "merging"} {
		key := seedLegionIssue(t, database, "INFRA", number+1, "Issue "+phase, fmt.Sprintf("r%d", number), nil, "")
		role := map[string]string{
			"planning": "planner", "implementing": "implementer", "testing": "tester",
			"reviewing": "reviewer", "accepting": "architect", "merging": "merger",
		}[phase]
		issues[key] = running{phase: phase, role: role, slot: number}
	}
	changed := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Second)
	seq := 0
	snapshot := func() []map[string]any {
		var events []map[string]any
		add := func(kind, issue string, value any) {
			seq++
			events = append(events, map[string]any{"seq": seq, "at": changed, "kind": kind, "issue": issue, "value": value})
		}
		for key, issue := range issues {
			add("phase", key, issue.phase)
			add("slot", key, issue.slot)
			add("claim_state", key, map[string]string{"role": issue.role, "state": "working"})
			// A retired claim of another role stays carried and is not a turn.
			add("claim_state", key, map[string]string{"role": "planner-old", "state": "retired"})
		}
		return events
	}
	check := func(when string) {
		t.Helper()
		board := legionBoardOf(t, handler, "INFRA")
		for key, issue := range issues {
			card := legionCard(t, board, key)
			if card.Phase != issue.phase || card.Slot == nil || *card.Slot != issue.slot ||
				!slices.Equal(card.Roles, []string{issue.role}) {
				t.Errorf("%s: %s is phase %q slot %v roles %v; want %q, slot %d, roles [%s]",
					when, key, card.Phase, card.Slot, card.Roles, issue.phase, issue.slot, issue.role)
			}
			if card.PhaseSince == nil || !card.PhaseSince.Equal(changed) {
				t.Errorf("%s: %s's phase_since = %v, want the change time %s", when, key, card.PhaseSince, changed)
			}
		}
		if got := board.Header.SlotsInUse; got != len(issues) {
			t.Errorf("%s: %d slots in use, want %d", when, got, len(issues))
		}
	}

	legionPush(t, handler, map[string]any{"project": "INFRA", "generation": "g1", "snapshot": true, "events": snapshot()})
	check("after the boot snapshot")
	legionPush(t, handler, map[string]any{"project": "INFRA", "generation": "g1", "snapshot": true, "events": snapshot()})
	check("after a repair snapshot")
	legionPush(t, handler, map[string]any{"project": "INFRA", "generation": "g1", "events": []map[string]any{
		{"seq": 0, "at": time.Now().UTC(), "kind": "heartbeat", "value": map[string]int{"last_seq": seq}},
	}})
	check("after a heartbeat")
	// Pushed moves after the snapshot finish every issue and free its slot and its turn...
	var moves []map[string]any
	for key, issue := range issues {
		seq++
		moves = append(moves, map[string]any{"seq": seq, "at": time.Now().UTC(), "kind": "phase", "issue": key, "value": "done"})
		seq++
		moves = append(moves, map[string]any{"seq": seq, "at": time.Now().UTC(), "kind": "slot", "issue": key, "value": nil})
		seq++
		moves = append(moves, map[string]any{"seq": seq, "at": time.Now().UTC(), "kind": "claim_state", "issue": key,
			"value": map[string]string{"role": issue.role, "state": "idle"}})
	}
	legionPush(t, handler, map[string]any{"project": "INFRA", "generation": "g1", "events": moves})
	if card := legionBoardOf(t, handler, "INFRA"); card.Header.SlotsInUse != 0 {
		t.Fatalf("after the moves %d slots are in use, want 0", card.Header.SlotsInUse)
	}
	// ...and a daemon that restarts onto its older state sends a new generation's snapshot whose
	// change times are older than those moves: the snapshot is the current value, so every issue
	// is back in its phase, its slot and its turn, since the snapshot's own change time.
	seq = 0
	legionPush(t, handler, map[string]any{"project": "INFRA", "generation": "g2", "snapshot": true, "events": snapshot()})
	check("after a restart's snapshot onto older state")
}
