package daemon

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

type taskStandingRecords struct {
	record.Store
	issue record.Issue
	rows  []record.PhaseRow
}

func (s taskStandingRecords) Issue(context.Context, pgx.Tx, string) (*record.Issue, error) {
	return &s.issue, nil
}

func (s taskStandingRecords) Phases(context.Context, pgx.Tx, string) ([]record.PhaseRow, error) {
	return s.rows, nil
}

func TestTheUnderLockStartFenceReadsItsAgentsStepNotASiblingOrTheIssuesSummary(t *testing.T) {
	pool := isolatedOutboxPool(t)
	const role = claim.Role("checker")
	token, err := claim.AgentToken("legion", "LEGION-208", "checker")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		phase phase.Phase
		held  bool
		want  bool
	}{
		{name: "control: its own step still runs", phase: phase.Step, want: true},
		{name: "its own held step is not its sibling of the same role", phase: phase.Step, held: true},
		{name: "the issue summary does not hide its running task", phase: phase.Implementing, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issue := record.Issue{Key: "LEGION-208", Generation: 1, Phase: tc.phase, Running: []record.RunningStep{
				{Task: "a", Role: role, Agent: "checker", Phase: phase.Step, Held: tc.held},
				{Task: "b", Role: role, Agent: "checker-2", Phase: phase.Step},
			}}
			records := taskStandingRecords{issue: issue, rows: []record.PhaseRow{{Issue: issue.Key, Task: "a", Agent: "checker", Role: role, Claim: token, Entry: 2}}}
			runner := outbox{pool: pool, records: records}
			got, err := runner.stepStands(t.Context(), record.OutboxRow{ID: 1, Issue: issue.Key}, record.SuperviseRequest{
				Role: role, Claim: token, Phase: phase.Step, Generation: 1, Entry: 2})
			if err != nil || got != tc.want {
				t.Fatalf("the agent's start stands = %t, error %v, want %t", got, err, tc.want)
			}
		})
	}
}
