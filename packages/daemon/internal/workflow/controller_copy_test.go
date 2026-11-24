package workflow

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/plan"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A notice told to its recipient and to the controller (noticeWithController) is queued twice
// when it names a task's sub-owner (record.Notice.To): the notice's row names that claim, and the
// controller's copy, a conversion of the same notice, holds none, since a controller notice goes
// to the project's controller alone.
//
// The fault it catches is the controller's copy refused for the claim the notice names, which
// fails the fact that tells both (its control: the record of the base, whose validation refuses
// that copy).
func TestAControllersCopyOfANoticeToASubOwnerNamesNoClaim(t *testing.T) {
	pool := migratedPool(t)
	engine := stepEngine(nil)
	root := stepRoot(t, pool, phase.Implementing)
	subOwner, err := claim.AgentToken("legion", root.Key, plan.SubOwnerAgent("f"))
	if err != nil {
		t.Fatal(err)
	}
	notice := record.Notice{Kind: "held", Role: claim.RolePlanner, Phase: phase.Planning, Reason: "escalated", To: subOwner}

	onStored(t, pool, engine, root.Key, func(ctx context.Context, tx pgx.Tx, issue record.Issue) error {
		return engine.noticeWithController(ctx, tx, issue.Key, notice)
	})

	told := outboxOf(t, pool, string(record.OutboxKindNotice))
	if len(told) != 1 || told[0].issue != root.Key || told[0].payload["to"] != string(subOwner) {
		t.Fatalf("notice rows = %+v, want one on %s that names %s", told, root.Key, subOwner)
	}
	copies := outboxOf(t, pool, string(record.OutboxKindControllerNotice))
	if len(copies) != 1 || copies[0].issue != root.Key || copies[0].payload["kind"] != "held" {
		t.Fatalf("controller notice rows = %+v, want the one copy of the held notice on %s", copies, root.Key)
	}
	if to, names := copies[0].payload["to"]; names {
		t.Fatalf("the controller's copy names %v as the claim it goes to, want none", to)
	}
}
