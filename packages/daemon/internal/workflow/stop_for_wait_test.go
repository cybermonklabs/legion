package workflow

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// R2: under a slot rule whose wait stops the phase under way (record.SlotRule.WaitStopsPhase),
// the stop for a wait of an issue at admitted stops nobody. Admitted is the architect's own phase
// (phase.RoleFor), and the architect owns the whole issue rather than the phase, so
// Engine.StopForWait writes no supervise row for it there; an issue in implementing gets the
// implementer's suspend, the stop the rule asks for, which keeps the worker's session.
func TestAWaitThatStopsThePhaseStopsNoArchitectAtAdmitted(t *testing.T) {
	const admitted, implementing, blocker = "ACME-1", "ACME-2", "ACME-3"
	pool := migratedPool(t)
	rule := record.MainRule
	rule.Name, rule.WaitStopsPhase = "main with a wait that stops the phase", true
	now := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	engine := New(record.NewStore(), Config{Project: "ACME", Linger: time.Hour, SlotRule: rule, Clock: func() time.Time { return now }}, nil)
	issues := []record.Issue{
		{Key: admitted, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: admitted, Project: "ACME",
			Title: "a root writing its design", Phase: phase.Admitted, Generation: 1, Status: "in_progress", Rank: "U"},
		{Key: implementing, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Tree: implementing, Project: "ACME",
			Title: "a root under way", Phase: phase.Implementing, Generation: 1, Status: "in_progress", Rank: "V"},
	}
	for _, issue := range issues {
		seedIssue(t, pool, issue)
	}

	applyInTx(t, pool, engine, func(ctx context.Context, tx pgx.Tx) error {
		for _, issue := range issues {
			if err := engine.StopForWait(ctx, tx, issue, []string{blocker}); err != nil {
				return err
			}
		}
		return nil
	})

	if rows := issueOutboxOf(t, pool, admitted, "supervise"); len(rows) != 0 {
		t.Fatalf("the stop for the wait of %s at admitted queued %d supervise rows, want none: admitted is the architect's, and no worker of its own works it", admitted, len(rows))
	}
	rows := issueOutboxOf(t, pool, implementing, "supervise")
	got := make([]record.SuperviseRequest, 0, len(rows))
	for _, row := range rows {
		got = append(got, decodeSupervise(t, row))
	}
	want := []record.SuperviseRequest{{Op: "suspend", Tree: implementing, Role: claim.RoleImplementer, Generation: 1, Reason: implementing + " waits on " + blocker}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("the stop for the wait of %s in implementing queued supervise rows %+v, want the implementer's suspend alone: %+v", implementing, got, want)
	}
}
