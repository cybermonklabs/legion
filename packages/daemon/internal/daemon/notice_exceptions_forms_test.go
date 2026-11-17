package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testnats"
)

// A role lane's name has two forms while every reader takes both (dispatch://INFRA-3862): the old
// form names the role alone, notifications.role.<role>, and the new one its project first,
// notifications.role.<PROJECT>.<role>. The listener reports a failed forward on the lane's own
// name under notifications.envoy.exceptions., so the daemon hears and reads a report of either.

// A report of a failed forward on the architect's lane in the new form, the daemon's Dispatch
// project first, arrives over core NATS on that name's exceptions subject, and the subscription
// hears it there and queues the notice again, as it does a report on the old form.
func TestARoleLaneExceptionInTheNewFormQueuesTheNoticeAgain(t *testing.T) {
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	putOutboxIssue(t, pool, records, record.Issue{Key: "LEGION-1", Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets", Project: "LEGION", Tree: "LEGION-1", Title: "Root", Phase: phase.Implementing, Generation: 1, Status: "in_progress"})
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records, supervisor: sup, project: "legion", now: time.Now}
	conn := testnats.Connect(t, testnats.URL(t))
	failed := make(chan error, 1)
	sub, err := subscribeNoticeExceptions(conn, func(data []byte) {
		if err := runner.rehold(context.Background(), data); err != nil {
			failed <- err
		}
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	t.Cleanup(func() { _ = sub.Unsubscribe() })
	topic := "notifications.role.LEGION.legion-legion-legion-1-architect"
	report := laneReport{"evt-exception", "delivery_failed", topic, "design-approved on LEGION-1", record.Notice{Kind: "design-approved", Version: 3}, "legion-outbox:9", "ses_live"}
	if err := conn.Publish("notifications.envoy.exceptions."+topic, report.envelope(t)); err != nil {
		t.Fatalf("publish the report: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(queuedNotices(t, pool)) == 0 && time.Now().Before(deadline) {
		select {
		case err := <-failed:
			t.Fatalf("rehold: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
	}
	if got := queuedNotices(t, pool); len(got) != 1 || got[0] != `LEGION-1 {"kind": "design-approved", "resends": 1, "version": 3, "resend_of": 9}` {
		t.Fatalf("queued %v, want the design-approved notice of LEGION-1", got)
	}
}

// A report on an architect's lane in the new form is this daemon's only under its own Dispatch
// project: the same role token under another project, or under no project, is another lane's, and
// changes nothing, as a report of another project's architect in the old form changes nothing.
//
// Negative control, run when this was written: drop the project check from exceptionNotice; the
// rows under OTHER and under _ queue the notice.
func TestANewFormReportOnAnotherProjectsLaneChangesNothing(t *testing.T) {
	notice := record.Notice{Kind: "pr-blocked", Role: "architect", Reason: "review_round_cap"}
	for _, topic := range []string{
		"notifications.role.OTHER.legion-legion-legion-1-architect",
		"notifications.role._.legion-legion-legion-1-architect",
		"notifications.role.LEGION.legion-other-legion-1-architect",
	} {
		t.Run(topic, func(t *testing.T) {
			pool := isolatedOutboxPool(t)
			sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
			architectClaimOn(t, sup, "LEGION-1", supervise.StateWorking, "ses_live")
			runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: record.NewStore(), supervisor: sup, project: "legion", now: time.Now}
			report := laneReport{"evt-exception", "delivery_failed", topic, "pr-blocked on LEGION-2", notice, "legion-outbox:7", "ses_live"}
			if err := runner.rehold(context.Background(), report.envelope(t)); err != nil {
				t.Fatalf("rehold: %v", err)
			}
			if got := queuedNotices(t, pool); len(got) != 0 {
				t.Fatalf("queued %v, want nothing", got)
			}
		})
	}
}
