package sessionanswers

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

func TestMain(m *testing.M) { os.Exit(storetest.Main(m)) }

// The read of a session's message deliveries (S2) is an index-only scan of
// message_deliveries_session_created (0147) for its sent deliveries and of
// message_deliveries_session_kept (20261115130748) for its kept ones, never a read through
// message_deliveries_session (0051) of every delivery the session ever received. It is explained
// twice: as the custom plan a statement's first runs get, and as the generic plan Postgres caches
// for it afterwards, which knows neither the session nor the window's start.
func TestTheReadOfASessionsMessageDeliveriesReadsItsWindowsRange(t *testing.T) {
	database := storetest.Open(t)
	ctx := context.Background()
	var messageID string
	if err := database.Pool.QueryRow(ctx, `
		insert into messages (author, body, target) values ('{"kind":"user","id":"alice"}', 'Broadcast.', 'session:session-0')
		returning id
	`).Scan(&messageID); err != nil {
		t.Fatalf("seed message: %v", err)
	}
	// 4,000 deliveries over 20 sessions, one an hour back from now, as long-lived sessions'
	// histories of broadcast copies, one in every seven kept for a session that was away.
	if _, err := database.Pool.Exec(ctx, `
		insert into message_deliveries (message_id, attempt, delivery, session_id, state, created_at)
		select $1, n, 'btw', 'session-' || (n % 20), case when n % 7 = 0 then 'kept' else 'sent' end,
		       now() - n * interval '1 hour'
		from generate_series(1, 4000) n
	`, messageID); err != nil {
		t.Fatalf("seed deliveries: %v", err)
	}
	if _, err := database.Pool.Exec(ctx, `vacuum analyze message_deliveries`); err != nil {
		t.Fatalf("vacuum deliveries: %v", err)
	}

	tx, err := database.Pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin explain transaction: %v", err)
	}
	defer tx.Rollback(ctx)
	explain := func(statement string, args ...any) json.RawMessage {
		t.Helper()
		var plans []struct {
			Plan json.RawMessage `json:"Plan"`
		}
		if err := tx.QueryRow(ctx, "explain (format json) "+statement, args...).Scan(&plans); err != nil || len(plans) != 1 {
			t.Fatalf("explain %s: %v, want one plan", statement, err)
		}
		return plans[0].Plan
	}

	since := time.Now().Add(-72 * time.Hour)
	if custom := explain(query, "session-1", since, "notifications.agent.session-1"); !readsWindow(t, custom) {
		t.Errorf("the custom plan reads message deliveries other than by an index-only scan of each of %v:\n%s", deliveryIndexes, custom)
	}
	if _, err := tx.Exec(ctx, "prepare session_answers (text, timestamptz, text) as "+query); err != nil {
		t.Fatalf("prepare the read: %v", err)
	}
	if _, err := tx.Exec(ctx, "set local plan_cache_mode = force_generic_plan"); err != nil {
		t.Fatalf("force generic plans: %v", err)
	}
	if generic := explain(`execute session_answers('session-1', now() - interval '72 hours', 'notifications.agent.session-1')`); !readsWindow(t, generic) {
		t.Errorf("the generic plan reads message deliveries other than by an index-only scan of each of %v:\n%s", deliveryIndexes, generic)
	}
}

// deliveryIndexes are the partial indexes S2 reads a session's deliveries through, one per arm.
var deliveryIndexes = []string{"message_deliveries_session_created", "message_deliveries_session_kept"}

// readsWindow reports whether an EXPLAIN (FORMAT JSON) plan reads message_deliveries by an
// index-only scan of each of deliveryIndexes, and reads it no other way.
func readsWindow(t *testing.T, plan json.RawMessage) bool {
	t.Helper()
	reads := map[string]int{}
	if other := deliveryReads(t, plan, reads); other {
		return false
	}
	for _, index := range deliveryIndexes {
		if reads[index] == 0 {
			return false
		}
	}
	return true
}

// deliveryReads counts, into reads by index, the nodes under plan that read message_deliveries by
// an index-only scan of one of deliveryIndexes, and reports whether any node reads it another way.
func deliveryReads(t *testing.T, plan json.RawMessage, reads map[string]int) bool {
	t.Helper()
	var node struct {
		NodeType  string            `json:"Node Type"`
		Relation  string            `json:"Relation Name"`
		IndexName string            `json:"Index Name"`
		Plans     []json.RawMessage `json:"Plans"`
	}
	if err := json.Unmarshal(plan, &node); err != nil {
		t.Fatalf("decode plan node: %v", err)
	}
	if node.Relation == "message_deliveries" {
		if node.NodeType == "Index Only Scan" && slices.Contains(deliveryIndexes, node.IndexName) {
			reads[node.IndexName]++
			return false
		}
		return true
	}
	other := false
	for _, child := range node.Plans {
		other = deliveryReads(t, child, reads) || other
	}
	return other
}
