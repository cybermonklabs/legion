package main

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/store/storetest/lockstep"
)

// TestTheSeedsDeliveryWriteTakesTheStoreLockOrder holds one batch of the seed's delivery write
// (writeDeliveries) to the lock order written on api/inbox_read.go's markInboxReadThreads, against
// a real Postgres. A batch that answers an ask indexes the ask's parts again, and the window write
// that ends that index takes the ask's owner lock (indexAskParts, refs.WriteItemWindows), so the
// batch takes the owner lock of every ask it answers or declines first, in one store.LockOwners
// call, as writeAsks takes its asks' owners: before it reads or writes the asks row, the ask's
// index or the events table.
//
// The batch runs through the real plan. The fixture's issue holds two open asks with no block,
// created long before the old session's span and written by writeAsks first, and the plan names
// one session and two deliveries in the window: planDeliveries takes both asks, the first turn
// answering one and the second, finding no comment and no message in the span, declining the
// other (deliveryPools.draw). The issue's owner lock, which owns both asks, is held for the whole
// run, and the batch is followed through it (lockstep.Follow): it must queue behind that lock at
// store.LockOwners' statement, holding no lock on asks, on the ask's index tables (ask_blocks,
// ask_block_state, ask_block_texts, ask_block_refs), on the tables its windows are written to
// (embedding_window_states, embeddings) or on events, and then finish. What it wrote is then read
// back - the answered ask holding the planned answer, the declined one the planned decline, and
// each delivery's event on the issue - so the run followed is the one that committed.
func TestTheSeedsDeliveryWriteTakesTheStoreLockOrder(t *testing.T) {
	ctx := context.Background()
	f := newSeedLockFixture(t)
	ask := func(number int) askPlan {
		return askPlan{
			ID:    fmt.Sprintf("5eed0000-0000-4000-c000-%012d", number),
			Issue: 0, Document: -1, BlockDoc: -1, Options: 2, State: "open",
			CreatedAt: baseTime.Add(time.Duration(number) * time.Hour),
		}
	}
	f.run.plan.asks = []askPlan{ask(1), ask(2)}
	f.run.plan.sessions = []string{"seed-lock-session"}
	f.run.sizes = sizes{WindowDeliveries: 2, OldSessionWindowDeliveries: 2}
	if err := f.run.writeAsks(ctx); err != nil {
		t.Fatalf("write the asks the deliveries answer and decline: %v", err)
	}
	deliveries := f.run.planDeliveries()
	types := make([]string, 0, len(deliveries))
	for _, planned := range deliveries {
		types = append(types, planned.eventType)
	}
	slices.Sort(types)
	if !slices.Equal(types, []string{"ask.answered", "ask.resolved"}) {
		t.Fatalf("planDeliveries planned %v, want one ask.answered and one ask.resolved: the batch followed here has to answer an ask, whose window write takes the ask's owner lock",
			types)
	}

	run := "the delivery batch"
	owner := lockstep.HoldOwner(t, f.database, &lockstep.Hold{
		Name: issueOwnerLock.name, Step: lockstep.StepOwner, Taken: true,
		Statement: lockstep.OwnerLockStatement,
		Unlocked: []string{
			"asks", "ask_blocks", "ask_block_state", "ask_block_texts", "ask_block_refs",
			"embedding_window_states", "embeddings", "events",
		},
	}, issueOwnerLock.owner)
	pid, done := f.startWrite(t, f.run.writeDeliveries)
	lockstep.Follow(t, f.database, run, pid, done, []*lockstep.Hold{owner})

	for _, planned := range deliveries {
		id := f.run.plan.asks[planned.item].ID
		state := "answered"
		if planned.eventType == "ask.resolved" {
			state = "resolved"
		}
		var held, words string
		if err := f.database.Pool.QueryRow(ctx,
			`select state, coalesce(answer->>'text', resolution->>'reason', '') from asks where id = $1`,
			id).Scan(&held, &words); err != nil {
			t.Fatalf("read back ask %s: %v", id, err)
		}
		if held != state || words != planned.words {
			t.Errorf("%s left ask %s %s with the words %q, want %s with %q (%s)",
				run, id, held, words, state, planned.words, planned.eventType)
		}
		var written int
		if err := f.database.Pool.QueryRow(ctx, `
			select count(*) from events
			where issue_key = $1 and type = $2 and payload->>'id' = $3 and payload->>'state' = $4
		`, seedLockIssue, planned.eventType, id, state).Scan(&written); err != nil {
			t.Fatalf("read back the %s event of ask %s: %v", planned.eventType, id, err)
		}
		if written != 1 {
			t.Errorf("%s wrote %d %s events of ask %s on %s, want 1", run, written, planned.eventType, id, seedLockIssue)
		}
	}
}
