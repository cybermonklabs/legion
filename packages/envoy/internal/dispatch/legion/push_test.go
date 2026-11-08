package legion

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// TestApplyPush covers the route's store: events at their own time, a repeated seq stored once, a
// gap holding last_seq back, and a snapshot repairing it and clearing what it did not carry.
func TestApplyPush(t *testing.T) {
	database, _ := configured(t, "http://daemon.example")
	ctx := context.Background()
	base := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	event := func(seq int64, offset time.Duration, kind, issue, value string) PushEvent {
		return PushEvent{Seq: seq, At: base.Add(offset), Kind: kind, Issue: issue, Value: json.RawMessage(value)}
	}
	apply := func(push Push) int64 {
		t.Helper()
		push.Project, push.Generation = "INFRA", "g1"
		last, err := ApplyPush(ctx, database.Pool, push)
		if err != nil {
			t.Fatalf("ApplyPush: %v", err)
		}
		return last
	}
	phaseSince := func(issue string) (string, time.Time) {
		t.Helper()
		var value string
		var at time.Time
		if err := database.Pool.QueryRow(ctx, `
			select value #>> '{}', observed_at from legion_observations
			where project = 'INFRA' and issue_key = $1 and field = 'phase'
			order by observed_at desc limit 1`, issue).Scan(&value, &at); err != nil {
			t.Fatalf("read the phase of %s: %v", issue, err)
		}
		return value, at
	}

	last := apply(Push{Snapshot: true, Events: []PushEvent{
		event(1, 0, KindPhase, "INFRA-1", `"planning"`),
		event(2, 0, KindPhase, "INFRA-2", `"implementing"`),
		event(3, 0, KindClaimState, "INFRA-2", `{"role":"implementer","state":"working"}`),
	}})
	if last != 3 {
		t.Fatalf("snapshot last_seq = %d, want 3", last)
	}
	// Two changes of one field in one batch both land, each at its own second.
	last = apply(Push{Events: []PushEvent{
		event(4, 1500*time.Millisecond, KindPhase, "INFRA-1", `"implementing"`),
		event(5, 7*time.Second, KindPhase, "INFRA-1", `"testing"`),
	}})
	if phase, at := phaseSince("INFRA-1"); last != 5 || phase != "testing" || !at.Equal(base.Add(7*time.Second)) {
		t.Fatalf("after batch: last_seq %d, phase %s since %s", last, phase, at)
	}
	// A repeated seq changes nothing.
	if last = apply(Push{Events: []PushEvent{event(5, 9*time.Second, KindPhase, "INFRA-1", `"reviewing"`)}}); last != 5 {
		t.Fatalf("repeat last_seq = %d", last)
	}
	if phase, _ := phaseSince("INFRA-1"); phase != "testing" {
		t.Fatalf("a repeated seq moved the phase to %s", phase)
	}
	// seq 6 lost: 7 is stored but last_seq stays at 5.
	if last = apply(Push{Events: []PushEvent{event(7, 10*time.Second, KindPhase, "INFRA-2", `"testing"`)}}); last != 5 {
		t.Fatalf("gap last_seq = %d, want 5", last)
	}
	// The snapshot repairs it and clears INFRA-2's claim, which it does not carry.
	last = apply(Push{Snapshot: true, Events: []PushEvent{
		event(8, 20*time.Second, KindPhase, "INFRA-1", `"reviewing"`),
		event(9, 20*time.Second, KindPhase, "INFRA-2", `"testing"`),
	}})
	if last != 9 {
		t.Fatalf("repair last_seq = %d, want 9", last)
	}
	var claim string
	if err := database.Pool.QueryRow(ctx, `
		select value::text from legion_observations
		where project = 'INFRA' and issue_key = 'INFRA-2' and field = 'claim.implementer'
		order by observed_at desc limit 1`).Scan(&claim); err != nil || claim != "null" {
		t.Fatalf("INFRA-2's claim after the snapshot = %q, %v; want null", claim, err)
	}
	at, pushed, err := PushFreshness(ctx, database.Pool, "INFRA")
	if err != nil || !pushed || !at.Equal(base.Add(20*time.Second)) {
		t.Fatalf("freshness = %s %v %v", at, pushed, err)
	}
}
