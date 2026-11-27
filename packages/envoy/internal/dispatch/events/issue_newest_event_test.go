package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestAnIssueAppendWritesTheNewestEventPairOnTheIssueRow holds the append (Broker.Append, which
// queues QueueAppendWithLogins) to writing, in the `update issues` that advances last_seq, the
// appended event's created_at and seq as the issue's last_event_at and last_event_at_seq
// (20261125143901). The priorities read takes each member issue's newest event from that pair and
// probes the event only where the pair does not name last_seq (api's priorityReadQuery), so an
// append that stopped writing it would leave every answer right and only the read's probes would
// show it. Two appends to one issue, each in a transaction of its own, each leave the issue's pair
// naming the event just appended, read back from the events row of the issue's last_seq; an issue
// no append reached keeps both null.
func TestAnIssueAppendWritesTheNewestEventPairOnTheIssueRow(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	database := storetest.Open(t)
	seedEventOwners(t, database)
	broker := NewBroker()
	for range 2 {
		appended := appendEvent(t, database, broker, issueUpdated("PP-1"))
		var lastSeq int
		var pairSeq *int
		var pairAt, eventAt *time.Time
		if err := database.Pool.QueryRow(ctx, `
			select i.last_seq, i.last_event_at_seq, i.last_event_at, e.created_at
			from issues i left join events e on e.issue_key = i.key and e.seq = i.last_seq
			where i.key = 'PP-1'
		`).Scan(&lastSeq, &pairSeq, &pairAt, &eventAt); err != nil {
			t.Fatalf("read PP-1's row after an append: %v", err)
		}
		if lastSeq != appended.Seq || pairSeq == nil || *pairSeq != appended.Seq ||
			pairAt == nil || eventAt == nil || !pairAt.Equal(*eventAt) || !pairAt.Equal(appended.CreatedAt) {
			t.Fatalf("after an append to PP-1 at seq %d (created_at %s), its row holds last_seq %d and the pair (last_event_at_seq %s, last_event_at %s), want the appended event's seq and created_at",
				appended.Seq, appended.CreatedAt.Format(time.RFC3339Nano), lastSeq, shown(pairSeq), shown(pairAt))
		}
	}
	var pairSeq *int
	var pairAt *time.Time
	if err := database.Pool.QueryRow(ctx, `select last_event_at_seq, last_event_at from issues where key = 'PP-2'`).Scan(&pairSeq, &pairAt); err != nil {
		t.Fatalf("read PP-2's row: %v", err)
	}
	if pairSeq != nil || pairAt != nil {
		t.Fatalf("PP-2, to which nothing appended, holds the pair (last_event_at_seq %s, last_event_at %s), want both null", shown(pairSeq), shown(pairAt))
	}
}

// shown is what p points to as a failure prints it, and "null" for nil.
func shown[T any](p *T) string {
	if p == nil {
		return "null"
	}
	switch value := any(*p).(type) {
	case time.Time:
		return value.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(value)
	}
}
