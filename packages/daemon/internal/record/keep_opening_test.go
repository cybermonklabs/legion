package record

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A start of the work hold's opening (SuperviseRequest.Opening) that a later start of the same
// role takes out hands that start its sentences (EnqueueStart, keepOpening): the start written in
// its place carries the opening, and its task is those sentences in front of its own work. It
// hands nothing to a start of another role, to a start that carries an opening of its own, which
// is the newer, or where the role's claim was given the opening's task already (the claim's
// delivered rows). Each case runs on an issue of its own, so one store serves them all.
func TestEnqueueStartKeepsTheOpeningOfTheStartItTakesOut(t *testing.T) {
	ctx := context.Background()
	st := migratedStore(t)
	records := NewStore()
	const opening = "The work hold stopped your turn. The design gate of the tree opened."
	const newer = "The work hold stopped your turn again. The design gate of the tree opened again."
	start := func(issue string, role claim.Role, at phase.Phase, task, opening string) OutboxRow {
		t.Helper()
		row, err := NewOutboxRow(issue, SuperviseRequest{Op: "start", Tree: issue, Role: role, Phase: at, Generation: 1, Task: task,
			Opening: opening, NeedsSlot: true}, time.Now().UTC())
		if err != nil {
			t.Fatalf("build the start of %s's %s: %v", issue, role, err)
		}
		return row
	}
	for _, tc := range []struct {
		name, issue string
		later       OutboxRow
		// given is whether the role's claim was given the opening's task before the later start.
		given                 bool
		wantOpening, wantTask string
		wantRole              claim.Role
	}{
		{name: "a later start of the same role", issue: "LEGION-208",
			later:       start("LEGION-208", claim.RoleImplementer, phase.Implementing, "Implement it again.", ""),
			wantOpening: opening, wantTask: supervise.OpeningTask(opening, "Implement it again."), wantRole: claim.RoleImplementer},
		{name: "a start of another role", issue: "LEGION-209",
			later:    start("LEGION-209", claim.RoleTester, phase.Testing, "Test it.", ""),
			wantTask: "Test it.", wantRole: claim.RoleTester},
		{name: "a start that carries an opening of its own", issue: "LEGION-210",
			later:       start("LEGION-210", claim.RoleImplementer, phase.Implementing, supervise.OpeningTask(newer, "Resume it."), newer),
			wantOpening: newer, wantTask: supervise.OpeningTask(newer, "Resume it."), wantRole: claim.RoleImplementer},
		{name: "a claim given the opening's task already", issue: "LEGION-211", given: true,
			later:    start("LEGION-211", claim.RoleImplementer, phase.Implementing, "Implement it again.", ""),
			wantTask: "Implement it again.", wantRole: claim.RoleImplementer},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inTx(t, st, func(tx pgx.Tx) {
				must(t, records.PutIssue(ctx, tx, issueFixture(tc.issue)))
				must(t, records.Enqueue(ctx, tx, start(tc.issue, claim.RoleImplementer, phase.Implementing,
					supervise.OpeningTask(opening, "Resume the work."), opening)))
				if tc.given {
					if _, err := tx.Exec(ctx, `insert into claims (token, project, tree, issue, role, generation, session, session_file,
						state, launch_failures, prompt_failures, prompt_retires, uncertain_streak, delivered_rows)
						select $1, 'LEGION', $2, $2, $3, 1, 'ses-implementer', '', $4, 0, 0, 0, 0, array[id]
						from outbox where issue = $2 and kind = 'supervise'`,
						"claim-"+tc.issue, tc.issue, string(claim.RoleImplementer), string(supervise.StateReady)); err != nil {
						t.Fatalf("write the claim given the opening's task: %v", err)
					}
				}
				must(t, records.EnqueueStart(ctx, tx, tc.later))
				var payloads []string
				if err := tx.QueryRow(ctx, `select array_agg(payload::text order by id) from outbox where issue = $1 and kind = 'supervise'`,
					tc.issue).Scan(&payloads); err != nil {
					t.Fatalf("read the starts of %s: %v", tc.issue, err)
				}
				if len(payloads) != 1 {
					t.Fatalf("the starts of %s are %q, want the later start alone, in the opening's start's place", tc.issue, payloads)
				}
				var got SuperviseRequest
				if err := json.Unmarshal([]byte(payloads[0]), &got); err != nil {
					t.Fatalf("decode the start of %s: %v", tc.issue, err)
				}
				if got.Role != tc.wantRole || got.Opening != tc.wantOpening || got.Task != tc.wantTask {
					t.Fatalf("the start of %s is the %s's with the opening %q and the task %q, want the %s's with the opening %q and the task %q",
						tc.issue, got.Role, got.Opening, got.Task, tc.wantRole, tc.wantOpening, tc.wantTask)
				}
			})
		})
	}
}
