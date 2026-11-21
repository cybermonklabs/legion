package daemon

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/issuelock"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A status row queued while the issue stood in accepting with Dispatch at its sign-off's retro —
// the status repair's needs_review — runs after the sign-off's fact has recorded the acceptance.
// Dispatch shows the retro the daemon records, so the row writes nothing: written, it would show
// the issue unaccepted while the daemon records it accepted. The control is the same row while the
// record still holds the issue in accepting, which writes.
func TestAStatusRowWritesNothingOverAnAcceptanceTheRecordHolds(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	for _, tc := range []struct {
		key    string
		at     phase.Phase
		writes []string
	}{
		{key: "LEGION-781", at: phase.Retro},
		{key: "LEGION-782", at: phase.Merging},
		{key: "LEGION-783", at: phase.Accepting, writes: []string{"needs_review"}},
	} {
		t.Run(string(tc.at), func(t *testing.T) {
			putOutboxIssue(t, pool, records, record.Issue{Key: tc.key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
				Project: "LEGION", Tree: tc.key, Title: tc.key, Phase: tc.at, Generation: 1, Status: "retro", DispatchStatus: "retro"})
			client := &outboxDispatch{issue: dispatch.Issue{Key: tc.key, Status: "retro", LastSeq: 9}}
			runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: &issuelock.Locks{}}
			if err := runner.execute(context.Background(), mustOutboxRow(t, tc.key, record.OrdinaryStatusWrite("needs_review", "retro", ""), time.Now())); err != nil {
				t.Fatalf("execute the row: %v", err)
			}
			if !slices.Equal(client.statuses, tc.writes) {
				t.Fatalf("Dispatch writes with the record in %s = %q, want %q", tc.at, client.statuses, tc.writes)
			}
		})
	}
}

// A status row of an issue whose acceptance holds its lock waits for the acceptance to end before
// it reads or writes anything of Dispatch, so it never writes between the acceptance's own write
// and its fact.
func TestAStatusRowWaitsForAnAcceptanceOfItsIssue(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const key = "LEGION-784"
	putOutboxIssue(t, pool, records, record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: key, Phase: phase.Testing, Generation: 1, Status: "testing", DispatchStatus: "in_progress"})
	client := &outboxDispatch{issue: dispatch.Issue{Key: key, Status: "in_progress", LastSeq: 4}}
	locks := &issuelock.Locks{}
	runner := &outbox{pool: pool, records: records, dispatch: client, log: quietLogger(), issueLocks: locks}

	accepted := locks.Accept(key)
	done := make(chan error, 1)
	go func() {
		done <- runner.execute(context.Background(), mustOutboxRow(t, key, record.OrdinaryStatusWrite("testing", "in_progress", ""), time.Now()))
	}()
	select {
	case err := <-done:
		accepted()
		t.Fatalf("the row ran while an acceptance of its issue held the lock: %v, writes %q", err, client.statuses)
	case <-time.After(2 * time.Second):
	}
	accepted()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute the row: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the row did not run within 30 seconds of the acceptance's end")
	}
	if want := []string{"testing"}; !slices.Equal(client.statuses, want) {
		t.Fatalf("Dispatch writes = %q, want %q once the acceptance ended", client.statuses, want)
	}
}
