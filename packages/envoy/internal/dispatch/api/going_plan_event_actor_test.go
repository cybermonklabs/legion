package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/sjawhar/envoy/internal/dispatch/model"
)

// TestThePlanEventNamesThePersonAndNoRun: going_plan.updated, which the event log hands every
// reader, names the person whose plans changed, through the command bar, and not the run whose
// plan changed, as the web's reader of it says (event-refresh.ts: the event names the person
// alone, and no run): bob's read of the log holds alice's event, whose actor is alice through the
// command bar with no run, and no text of the run's id.
func TestThePlanEventNamesThePersonAndNoRun(t *testing.T) {
	t.Parallel()
	rig := newGoingWritesRig(t)
	ctx := t.Context()
	run := uuid.NewString()
	tx, err := rig.database.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rig.served.appendGoingPlanEvent(ctx, tx, "alice", run); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("append the plan's event: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	response := dispatchRequest(t, rig.mux, http.MethodGet, "/api/v1/events/log?limit=1000", nil, "bob")
	if response.Code != http.StatusOK {
		t.Fatalf("bob's read of the event log: status=%d body=%s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), run) {
		t.Errorf("bob's read of the event log holds alice's run %s: %s", run, response.Body.String())
	}
	found := false
	for _, event := range decodeBody[[]struct {
		Type  string      `json:"type"`
		Actor model.Actor `json:"actor"`
	}](t, response) {
		if event.Type != "going_plan.updated" {
			continue
		}
		found = true
		actor := event.Actor
		if actor.Kind != "user" || actor.ID != "alice" || actor.Act == nil || actor.Act.Agent != model.ActAgentCommandBar || actor.Act.Run != "" {
			t.Errorf("the plan's event names the actor %+v (act %+v), want alice through the command bar and no run", actor, actor.Act)
		}
	}
	if !found {
		t.Fatalf("bob's read of the event log holds no going_plan.updated: %s", response.Body.String())
	}
}
