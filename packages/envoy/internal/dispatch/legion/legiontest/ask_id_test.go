package legiontest

import (
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/dispatch/legion/legionseed"
	"github.com/sjawhar/envoy/internal/dispatch/store/storetest"
)

// TestEveryAskTheRigWritesCarriesAnIdDispatchCanRead: the ask id in the rig's `ask.opened` and
// `ask.answered` events must be a uuid, because every reader of an ask casts it to that type —
// `where s.ask_id = any($1::uuid[])` in refs/ask_blocks.go's askPartRows is the one the event
// stream relay runs for each event it carries.
//
// The rig previously wrote `ask-<ISSUE>`, so a server that reads an event of it fails that cast
// and the relay never gets past it. Measured against a real Dispatch server of this head on
// 2026-11-11: with one page subscribed to the Legion view, seeding one project of this rig put
// the relay into a retry of one attempt a second that never advanced — 25 attempts in 25 s,
// each `read ask parts: ERROR: invalid input syntax for type uuid: "ask-BOLTS-1" (SQLSTATE
// 22P02)`. While it is stuck no event reaches the live hub, so every open page stops being
// updated.
//
// The cast is made here rather than by a reader of this package, because the cast is the whole
// of what a reader needs of these ids and the database is the one authority on the type.
func TestEveryAskTheRigWritesCarriesAnIdDispatchCanRead(t *testing.T) {
	database := storetest.Open(t)
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, time.UTC)
	Seed(t, database.Pool, legionseed.Spec{Project: "LEGION", Trees: 2, Now: now})

	rows, err := database.Pool.Query(t.Context(), `
		select distinct (payload ->> 'id')::uuid::text
		from events
		where issue_key like 'LEGION-%' and type in ('ask.opened', 'ask.answered')
		  and payload ? 'id'`)
	if err != nil {
		t.Fatalf("Dispatch cannot read the ask ids the rig wrote, which is what stops the event stream relay: %v", err)
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatalf("read the ask ids the rig wrote: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read the ask ids the rig wrote: %v", err)
	}
	if len(ids) == 0 {
		t.Fatalf("the rig wrote no ask event, so this test examined nothing")
	}
	t.Logf("%d ask ids examined, all of them readable as the uuid every reader casts them to", len(ids))
}
