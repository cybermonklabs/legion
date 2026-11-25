package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// A delivery's copy counts, and folds, only while the record still holds the run it was rendered
// in: the generation of each item's issue and of its tree's root (the SQL function
// design_news_current, record.DeliveryNewsItem.Generation and TreeGeneration). The reviewer's task
// told the child's holder version 5 of the spec and the opening, and the root's architect version 2
// of the Verification document, each rendered in generation 1 of its issue under a root on
// generation 1, and its turn confirmed it.
//
// Each case then moves the record as a clear of a generation leaves it, or holds a copy that never
// named a run. Where the child's own generation moved (a child set back to todo, an issue moved
// into another tree), what the copy told the child counts nothing, and what it told the root's
// architect still counts: the root stands in the run that item was rendered in. Where the root's
// generation moved (a root set back to todo), nothing of the copy counts: the root's own item by
// its generation, and the child's by its tree's. A copy whose items and opening name no run counts
// nothing, since no run is the one it was told to. In every case the fold of the turn's result
// writes what the read counted and no more, so the rows stand as the read left them.
func TestACopyCountsAndFoldsOnlyWhileTheRecordHoldsTheRunItWasRenderedIn(t *testing.T) {
	t.Parallel()
	childSeed := map[record.DesignStep]int{record.DesignStepSpec: 3}
	rootSeed := map[record.DesignStep]int{record.DesignStepSpec: 5}
	rootTold := map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}
	for _, tc := range []struct {
		name string
		// unmarked says the copy names no run; moved is the issue whose generation the record
		// raises after the confirmation, "" for none.
		unmarked bool
		moved    string
		// root is what the root's architect reads as having taken afterwards.
		root map[record.DesignStep]int
	}{
		{name: "the issue's own generation moved", moved: toldChild, root: rootTold},
		{name: "its tree root's generation moved", moved: toldRoot, root: rootSeed},
		{name: "the copy names no run", unmarked: true, root: rootSeed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newToldRig(t)
			r.seedTold()
			confirmed := r.newsTask("outbox:7")
			if tc.unmarked {
				var copied record.DeliveryNews
				if err := json.Unmarshal(confirmed.News, &copied); err != nil {
					t.Fatal(err)
				}
				// The copy as JSON that names neither member, which the struct always writes.
				plain := map[string]any{"task": copied.Task,
					"items": []map[string]any{
						{"issue": toldChild, "audience": "holder", "step": "spec", "artifact": "artifact-spec", "from": 3, "to": 5},
						{"issue": toldRoot, "audience": "architect", "step": "criteria", "artifact": "artifact-checks", "from": nil, "to": 2},
					},
					"openings": []map[string]any{{"issue": toldChild, "audience": "holder", "seq": 0}}}
				encoded, err := json.Marshal(plain)
				if err != nil {
					t.Fatal(err)
				}
				confirmed.News, confirmed.Carried = encoded, encoded
			}
			confirmed.ConfirmedAt = r.at.Add(time.Minute)
			if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
				t.Fatalf("confirm the task: %v", err)
			}
			if !tc.unmarked {
				r.wantTold("after the confirmation, in the run the copy was rendered in", toldChild, record.AudienceHolder,
					map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
				r.wantTold("after the confirmation, in the run the copy was rendered in", toldRoot, record.AudienceArchitect, rootTold, false)
			}
			if tc.moved != "" {
				r.exec(`update issues set generation = generation + 1 where key = $1`, tc.moved)
			}
			r.wantTold("while the credit stands", toldChild, record.AudienceHolder, childSeed, true)
			r.wantTold("while the credit stands", toldRoot, record.AudienceArchitect, tc.root, false)

			final := confirmed
			final.Ran = true
			if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, final); err != nil {
				t.Fatalf("record that the turn ran: %v", err)
			}
			if err := r.store.RetireDelivery(r.ctx, r.reviewer, final.ID); err != nil {
				t.Fatalf("retire the task: %v", err)
			}
			r.wantTold("after the fold and the retirement", toldChild, record.AudienceHolder, childSeed, true)
			r.wantTold("after the fold and the retirement", toldRoot, record.AudienceArchitect, tc.root, false)
		})
	}
}
