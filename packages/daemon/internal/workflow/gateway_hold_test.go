package workflow

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/backpressure"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// The catch-up a start writes in place of a launch is a paced wake of the architect, so while the
// model gateway's hold stands it is reserved from the hold's end (dispatch://INFRA-5802 R3, check
// c-hold-stops-starts): the row is due no earlier than the hold's end, carries that moment, and the
// hold counts the architect's wake. With no hold standing the same ready is due at once.
func TestHoldStopsStartsACatchUpUntilTheHoldEnds(t *testing.T) {
	now := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	until := now.Add(90 * time.Second)
	for _, tc := range []struct {
		name string
		held bool
		due  time.Time
	}{
		{name: "under a standing hold", held: true, due: until},
		{name: "with no hold standing", due: now},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			seedAdmittedTree(t, pool, 1)
			hold := backpressure.New("team-a", nil)
			if tc.held {
				hold.Apply([]backpressure.Object{{Team: backpressure.AllTeams, State: backpressure.StateHold, HoldUntil: until, At: now, Reason: "refusing"}}, now)
			}
			var reservedFrom []time.Time
			cfg := Config{Project: "LEGION", Steps: config.DesignGateRootIssues.Steps(), Repositories: testRepositories,
				Clock: func() time.Time { return now }, Hold: hold,
				Reserve: func(from time.Time) time.Time { reservedFrom = append(reservedFrom, from); return from }}
			engine := New(record.NewStore(), cfg, nil)
			token, err := claim.NewToken("legion", "LEGION-1", claim.RoleArchitect)
			if err != nil {
				t.Fatalf("claim token: %v", err)
			}

			if _, err := intake.ApplyFact(context.Background(), pool, "outbox", fmt.Sprintf("start-running:%s:1", token),
				intake.ClaimReady{Issue: "LEGION-1", Role: claim.RoleArchitect, Launch: 1, Paced: true}, engine, admissionStub{}); err != nil {
				t.Fatalf("apply the ready: %v", err)
			}

			var due, marked time.Time
			if err := pool.QueryRow(t.Context(), `select next_at, (payload->>'paced_at')::timestamptz from outbox
				where kind = 'notice' and payload->>'kind' = 'catch-up'`).Scan(&due, &marked); err != nil {
				t.Fatalf("read the catch-up: %v", err)
			}
			if !due.UTC().Equal(tc.due) || !marked.UTC().Equal(tc.due) || len(reservedFrom) != 1 || !reservedFrom[0].Equal(tc.due) {
				t.Fatalf("the catch-up is due at %s, marked %s, reserved from %v; want all %s", due.UTC(), marked.UTC(), reservedFrom, tc.due)
			}
			view, standing := hold.View(now)
			if standing != tc.held || (tc.held && view.Wakes != 1) {
				t.Fatalf("the hold's view is %+v (standing %t), want standing %t counting the architect's wake", view, standing, tc.held)
			}
		})
	}
}
