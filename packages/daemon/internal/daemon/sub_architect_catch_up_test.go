package daemon

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// subArchitectCatchUp is the catch-up of LEGION-2's own architect as the workflow composes it
// (workflow.QueueCatchUp): the child and the issue under it, no gate, for generation and launch.
func subArchitectCatchUp(generation, launch uint64) record.Notice {
	return record.Notice{Kind: "catch-up", Role: claim.RoleArchitect, Reason: fmt.Sprintf("the issues under LEGION-2 at generation %d, launch %d", generation, launch),
		CatchUp: &record.CatchUp{Generation: generation, Launch: launch, Gates: []record.CatchUpGate{}, Issues: []record.CatchUpIssue{
			{Key: "LEGION-2", Parent: "LEGION-1", Title: "Child", Phase: phase.Planning, Status: "in_progress"},
			{Key: "LEGION-3", Parent: "LEGION-2", Title: "Grandchild", Phase: phase.Testing, Status: "testing"},
		}}}
}

// subArchitectOn creates LEGION-2's architect claim in state at launch, suspended by by.
func subArchitectOn(t *testing.T, sup *supervisor, state supervise.ClaimState, launch uint64, by supervise.SuspendSource) {
	t.Helper()
	if _, _, err := sup.Create(context.Background(), supervise.Claim{
		Token: mustClaimToken(t, "LEGION-2", claim.RoleArchitect), Project: "legion", Tree: "LEGION-1", Issue: "LEGION-2", Role: claim.RoleArchitect,
		State: state, Session: "ses_sub", Generation: launch, SuspendedBy: by,
	}, ""); err != nil {
		t.Fatalf("create the sub-architect's claim: %v", err)
	}
}

// A child's catch-up states what the child's own architect owns, so it goes to that architect
// and to no other: it is published on LEGION-2's architect topic, with one line naming the claim
// it reached, its generation, its launch and how many issues it lists. When that claim has
// stopped, the catch-up is never published to the root's architect, which would read the child's
// state as its own: a claim that failed or retired finishes the row undelivered, since the start
// that brings it back (for a child's failed architect, the child's next phase start) queues a
// catch-up of its own at its ready, and one suspended while its issue is in the workflow holds
// it, as a root's catch-up is held, without holding back the root's own notices behind it. A
// child parked out of the workflow (backlog) has its architect suspended with its launch and the
// child's generation kept, so no newer catch-up supersedes the row and its claim has not ended:
// its catch-up finishes undelivered at once, with one line, publishing nothing and waiting for
// nothing, since the child's next launch writes a fresh one. Held, it would wait on its backoff,
// publish to a role nobody holds and warn, for as long as the child stayed parked. A child closed
// as done, whose architect the close retired, ends its catch-up the same way, before any publish.
// A newer launch of the child's architect, or the child at a later generation, supersedes an
// older catch-up, which finishes without publishing; the root's claim and generation decide
// nothing about it.
func TestASubArchitectsCatchUpGoesToItAlone(t *testing.T) {
	t.Parallel()
	t.Run("to the child's own architect", func(t *testing.T) {
		pool := isolatedOutboxPool(t)
		records := record.NewStore()
		noticeTree(t, pool, records, false)
		sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
		architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
		subArchitectOn(t, sup, supervise.StateReady, 1, "")
		enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", subArchitectCatchUp(1, 1), time.Now()))
		var logged bytes.Buffer
		publisher := &holderPublisher{}
		runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
			supervisor: sup, project: "legion", now: time.Now}

		if err := runner.RunOnce(context.Background()); err != nil {
			t.Fatalf("run the catch-up: %v", err)
		}
		if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[catch-up to LEGION-2]" || outboxRows(t, pool) != 0 {
			t.Fatalf("delivered %v with %d rows left; want the catch-up on the sub-architect's topic alone", deliveredKinds(t, delivered), outboxRows(t, pool))
		}
		var published []string
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			if strings.Contains(line, `msg="outbox notice published"`) {
				published = append(published, line)
			}
		}
		want := []string{"kind=catch-up", "issue=LEGION-2", "architect=" + string(mustClaimToken(t, "LEGION-2", claim.RoleArchitect)), "generation=1", "launch=1", "issues=2"}
		if len(published) != 1 || !containsAll(published[0], want) {
			t.Fatalf("published lines %q in log:\n%s\nwant one naming %v", published, logged.String(), want)
		}
	})

	t.Run("its architect stopped", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			state supervise.ClaimState
			by    supervise.SuspendSource
			// held is whether the catch-up still waits for the sub-architect after the ticks.
			held bool
		}{
			{name: "its claim failed", state: supervise.StateFailed},
			{name: "its claim retired", state: supervise.StateRetired},
			{name: "its claim suspended, its issue in the workflow", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon, held: true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				noticeTree(t, pool, records, false)
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
				subArchitectOn(t, sup, tc.state, 1, tc.by)
				enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", subArchitectCatchUp(1, 1), time.Now()))
				// A later notice the root owns, written after the catch-up: the catch-up held for
				// the child's architect holds nothing of the root's back.
				enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-1", record.Notice{Kind: "design-approved", Version: 2}, time.Now()))
				publisher := &holderPublisher{}
				publisher.setAbsent(architectTopic(t, "LEGION-2"))
				clock := time.Now()
				runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher, supervisor: sup,
					project: "legion", now: func() time.Time { return clock }}

				for tick := range 4 {
					if err := runner.RunOnce(context.Background()); err != nil {
						t.Fatalf("tick %d: %v", tick, err)
					}
					clock = clock.Add(2 * time.Minute)
				}
				rows := 0
				if tc.held {
					rows = 1
				}
				if _, delivered := publisher.snapshot(); fmt.Sprint(deliveredKinds(t, delivered)) != "[design-approved to LEGION-1]" || outboxRows(t, pool) != rows {
					t.Fatalf("delivered %v with %d rows left; want only the root's own notice at the root, and %d rows left", deliveredKinds(t, delivered), outboxRows(t, pool), rows)
				}
			})
		}
	})

	t.Run("its issue out of the workflow", func(t *testing.T) {
		for _, tc := range []struct {
			name, status string
			state        supervise.ClaimState
			by           supervise.SuspendSource
		}{
			// A park suspends the child's architect and keeps its claim (workflow's leave).
			{name: "parked to backlog", status: "backlog", state: supervise.StateSuspended, by: supervise.SuspendedByDaemon},
			// A close as done retires it, its session kept for a reopen (supervise's issue close).
			{name: "closed as done", status: "done", state: supervise.StateRetired},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				noticeTree(t, pool, records, false)
				// The child left the workflow (workflow's leave): its phase is done, and its
				// architect's claim stands at the catch-up's launch, with the child at the
				// catch-up's generation, so nothing supersedes the row.
				root := "LEGION-1"
				putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root,
					Parent: &root, Title: "Child", Phase: phase.Done, Generation: 1, Status: tc.status})
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
				subArchitectOn(t, sup, tc.state, 1, tc.by)
				enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", subArchitectCatchUp(1, 1), time.Now()))
				var logged bytes.Buffer
				publisher := &holderPublisher{}
				publisher.setAbsent(architectTopic(t, "LEGION-2"))
				clock := time.Now()
				runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
					supervisor: sup, project: "legion", now: func() time.Time { return clock }}

				// Eight ticks, each past the row's backoff: a row held for a suspended claim would
				// run on every one and warn from the seventh attempt (settle's waitWarnAttempts).
				for tick := range 8 {
					if err := runner.RunOnce(context.Background()); err != nil {
						t.Fatalf("tick %d: %v", tick, err)
					}
					clock = clock.Add(2 * time.Minute)
				}
				if attempts, delivered := publisher.snapshot(); attempts != 0 || len(delivered) != 0 || outboxRows(t, pool) != 0 {
					t.Fatalf("%d publishes, delivered %v with %d rows left, logged:\n%s\nwant the catch-up finished with no publish",
						attempts, deliveredKinds(t, delivered), outboxRows(t, pool), logged.String())
				}
				var finished []string
				for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
					if strings.Contains(line, `msg="outbox catch-up finished undelivered: its issue is out of the workflow"`) {
						finished = append(finished, line)
					}
				}
				want := []string{"issue=LEGION-2", "status=" + tc.status, "generation=1", "launch=1"}
				if len(finished) != 1 || !containsAll(finished[0], want) || strings.Contains(logged.String(), `msg="outbox row waits"`) {
					t.Fatalf("finished lines %q in log:\n%s\nwant one naming %v and no wait", finished, logged.String(), want)
				}
			})
		}
	})

	t.Run("a newer catch-up supersedes it", func(t *testing.T) {
		for _, tc := range []struct {
			name              string
			launch            uint64
			childGeneration   uint64
			catchUpGeneration uint64
			catchUpLaunch     uint64
		}{
			{name: "the child's architect ready at a later launch", launch: 3, childGeneration: 1, catchUpGeneration: 1, catchUpLaunch: 2},
			{name: "the child re-entered at a later generation", launch: 3, childGeneration: 2, catchUpGeneration: 1, catchUpLaunch: 3},
		} {
			t.Run(tc.name, func(t *testing.T) {
				pool := isolatedOutboxPool(t)
				records := record.NewStore()
				noticeTree(t, pool, records, false)
				root := "LEGION-1"
				putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-2", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: root,
					Parent: &root, Title: "Child", Phase: phase.Planning, Generation: tc.childGeneration, Status: "in_progress"})
				sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
				architectClaim(t, sup, "LEGION-1", supervise.StateWorking)
				subArchitectOn(t, sup, supervise.StateReady, tc.launch, "")
				enqueueOutbox(t, pool, records, mustOutboxRow(t, "LEGION-2", subArchitectCatchUp(tc.catchUpGeneration, tc.catchUpLaunch), time.Now()))
				var logged bytes.Buffer
				publisher := &holderPublisher{}
				runner := &outbox{log: slog.New(slog.NewTextHandler(&logged, nil)), pool: pool, dispatchProject: "LEGION", records: records, notices: publisher,
					supervisor: sup, project: "legion", now: time.Now}

				if err := runner.RunOnce(context.Background()); err != nil {
					t.Fatalf("run the catch-up: %v", err)
				}
				if _, delivered := publisher.snapshot(); len(delivered) != 0 || outboxRows(t, pool) != 0 ||
					!strings.Contains(logged.String(), `msg="outbox catch-up finished without publishing: a newer catch-up supersedes it"`) {
					t.Fatalf("delivered %v with %d rows left, logged:\n%s\nwant the older catch-up finished without publishing", deliveredKinds(t, delivered), outboxRows(t, pool), logged.String())
				}
			})
		}
	})
}

// containsAll is whether line holds every one of parts.
func containsAll(line string, parts []string) bool {
	for _, part := range parts {
		if !strings.Contains(line, part) {
			return false
		}
	}
	return true
}
