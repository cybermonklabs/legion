package record

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
)

// The start a hook prompt waits for (R1 of dispatch://INFRA-4480, Store.MoveStartQueued) is the
// start its move back queued: a start of the role of the prompt's To phase (phase.RoleFor) that
// names that phase, carries the slot mark workflow.StartOrOwe gives a start of it, and was written
// after the prompt. No other row of the issue holds the prompt: a start written before it, the
// resume task a ready worker is given (workflow's readyWorkerTask), which names the same phase and
// role and carries no slot mark, and a start of another phase.
//
// RoleFor answers the architect for admitted and accepting, the two phases the architect works.
// The read never meets admitted: a hook prompt names a move back between two phases of the forward
// line (phase.IsMovePair), which admitted is not, so the row is refused before it is written. For
// accepting it reads a start of the architect that names accepting with no slot mark, which no
// transition writes (workflow's transition starts no worker of accepting), so it answers false and
// the prompt waits for no start: accepting keeps no slot, and its architect is no phase worker.
func TestAMoveStartQueuedHoldsAHookPromptOnlyForTheStartItsMoveBackQueued(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	now := time.Now().UTC()
	start := func(issue string, role claim.Role, ph phase.Phase, slot bool) OutboxRow {
		t.Helper()
		row, err := NewOutboxRow(issue, SuperviseRequest{Op: "start", Tree: issue, Role: role, Phase: ph, Generation: 1,
			Task: "Work the phase.", NeedsSlot: slot}, now)
		if err != nil {
			t.Fatalf("build a start of %s's %s: %v", issue, role, err)
		}
		return row
	}
	prompt := func(issue string, from, to phase.Phase) OutboxRow {
		t.Helper()
		row, err := NewOutboxRow(issue, HookPrompt{Hook: "work-back", Role: claim.RoleImplementer, From: from, To: to, Count: 1,
			Counts: map[string]int{phase.Pair(from, to): 1}, Prompt: "Read the thread."}, now)
		if err != nil {
			t.Fatalf("build a hook prompt of %s: %v", issue, err)
		}
		return row
	}

	for _, tc := range []struct {
		name   string
		to     phase.Phase
		before []OutboxRow
		after  []OutboxRow
		want   bool
	}{
		{name: "the start the move back queued", to: phase.Implementing,
			after: []OutboxRow{start("LEGION-301", claim.RoleImplementer, phase.Implementing, true)}, want: true},
		{name: "a start written before the prompt", to: phase.Implementing,
			before: []OutboxRow{start("LEGION-302", claim.RoleImplementer, phase.Implementing, true)}},
		{name: "a ready worker's resume task, with no slot mark", to: phase.Implementing,
			after: []OutboxRow{start("LEGION-303", claim.RoleImplementer, phase.Implementing, false)}},
		{name: "a start of another phase", to: phase.Implementing,
			after: []OutboxRow{start("LEGION-304", claim.RoleTester, phase.Testing, true)}},
		{name: "a move back into accepting, the architect's phase", to: phase.Accepting,
			after: []OutboxRow{start("LEGION-305", claim.RoleImplementer, phase.Implementing, true)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := "LEGION-301"
			switch {
			case len(tc.after) > 0:
				issue = tc.after[0].Issue
			case len(tc.before) > 0:
				issue = tc.before[0].Issue
			}
			from := phase.Reviewing
			if tc.to == phase.Accepting {
				from = phase.Merging
			}
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, issueFixture(issue)))
				for _, row := range tc.before {
					must(t, records.Enqueue(ctx, tx, row))
				}
				id, err := records.EnqueueID(ctx, tx, prompt(issue, from, tc.to))
				must(t, err)
				for _, row := range tc.after {
					must(t, records.Enqueue(ctx, tx, row))
				}
				got, err := records.MoveStartQueued(ctx, tx, issue, id, tc.to)
				must(t, err)
				if got != tc.want {
					t.Fatalf("MoveStartQueued for a move back into %s = %t, want %t", tc.to, got, tc.want)
				}
			})
		})
	}

	// A hook prompt of a move into admitted, the other phase RoleFor answers the architect for, is no
	// row the outbox can hold, so MoveStartQueued is never asked about admitted.
	if _, err := NewOutboxRow("LEGION-306", HookPrompt{Hook: "work-back", Role: claim.RoleImplementer, From: phase.Implementing,
		To: phase.Admitted, Count: 1, Prompt: "Read the thread."}, now); err == nil {
		t.Fatalf("a hook prompt of a move into admitted was written, want it refused: admitted is no phase of the forward line")
	}
}
