package store

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// A fix's read of its failed CI job (record.FixJobRead) reaches no session itself, but its answer
// is what starts the fix's task on the implementer, so the row holds the issue's pod as the
// merge-queue read's does (idleWaitingKinds): the idle view reads it waiting, whether it is due now
// or backing off behind a Dispatch that is down, and one queued after a release's rows cancels the
// release at its boundary. The control is a step record queued the same way, which holds no pod and
// lets the boundary act: the reads tell a row that waits for a role of the pod from one that does
// not.
func TestAPendingFixJobReadKeepsThePod(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attempts int
		nextAt   string
	}{
		{name: "due now", nextAt: "now()"},
		{name: "backing off behind a Dispatch that is down", attempts: 6, nextAt: "now() + interval '1 hour'"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := migratedStore(t)
			ctx := context.Background()
			openRunningTree(t, st)
			if err := st.PutClaim(ctx, supervise.Claim{Token: claim.Token("legion-legion-legion-208-implementer"),
				Project: "legion", Tree: rootIssue, Issue: rootIssue, Role: claim.RoleImplementer, TreeEpoch: 1,
				Generation: 1, ServingGeneration: 1, State: supervise.StateIdle}); err != nil {
				t.Fatal(err)
			}
			queue := func(payload record.OutboxPayload) {
				t.Helper()
				row, err := record.NewOutboxRow(rootIssue, payload, time.Now().UTC())
				if err != nil {
					t.Fatal(err)
				}
				if _, err := st.Pool().Exec(ctx, `insert into outbox (kind, issue, payload, attempts, next_at, last_error)
					values ($1, $2, $3, $4, `+tc.nextAt+`, 'Dispatch is unreachable')`,
					string(row.Kind), rootIssue, row.Payload, tc.attempts); err != nil {
					t.Fatal(err)
				}
			}
			started := time.Now().UTC()
			stepRecord := record.StepRecord{ID: "0b9e5c2a-7f43-5d1e-9a6b-3c4d5e6f7a8b", Task: "default", Step: 3, Kind: "step",
				Role: "reviewer", Type: "review", Agent: "legion-legion-legion-208-reviewer", Generation: 1, StartedAt: &started}
			read := record.FixJobRead{Generation: 1, Task: "default", Fix: 1, Check: "k-ci", Head: "c0de000000000000000000000000000000000001"}

			queue(stepRecord)
			if view := idleViewOf(t, st, rootIssue); view.Waiting {
				t.Fatal("control: the idle view reads a step record as waiting for a role of the pod")
			}
			queue(read)
			if view := idleViewOf(t, st, rootIssue); !view.Waiting {
				t.Fatal("the idle view reads nothing waiting, though a fix's read of its CI job is queued: a release would stop the implementer before the fix starts")
			}
			// A row that waits for a role of the pod overtakes a release from below its rows as
			// well as from above them, so the release below starts on an issue whose rows the view
			// read are gone, as an idle pod's are when the tick releases it.
			if _, err := st.Pool().Exec(ctx, `delete from outbox where issue = $1`, rootIssue); err != nil {
				t.Fatal(err)
			}
			_, boundary, _ := releaseThroughTheStore(t, st, rootIssue)
			queue(stepRecord)
			if decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary); err != nil || decision.Act != ReleaseActs {
				t.Fatalf("control: the boundary with a step record queued after the release's rows = %s for %s, %v; want %s",
					decision.Act, decision.Cause, err, ReleaseActs)
			}
			queue(read)
			decision, err := st.IssueReleaseStop(ctx, "legion", rootIssue, boundary)
			if err != nil || decision.Act != ReleaseCancel || decision.Cause != ReleaseCauseOvertaken {
				t.Fatalf("the boundary with a fix's read of its CI job queued after the release's rows = %s for %s, %v; want %s for %s",
					decision.Act, decision.Cause, err, ReleaseCancel, ReleaseCauseOvertaken)
			}
		})
	}
}
