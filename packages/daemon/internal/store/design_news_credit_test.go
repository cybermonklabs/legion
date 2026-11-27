package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/record"
)

// Two confirmations of one task can reach the store as one write of confirmed over confirmed. The
// store holds the delivery confirmed with copy C1; the machine takes that confirmation back in
// memory (a refused send, which keeps the id), that write fails and is carried past, and settle's
// repair of it fails too; a new turn confirms the task again with copy C2, and that write lands.
// The credit is the delivery's own, so the write of C2 ends C1's credit and makes C2's, whatever
// the row it replaces held: the holder reads as told what C2's prompt carried and nothing of C1,
// and the take-back of C2 leaves the row as it stood before either. The same confirmation written
// a second time changes nothing.
func TestASecondConfirmationOverAConfirmationWhoseTakeBackWasLostCreditsOnlyItsOwnCopy(t *testing.T) {
	t.Parallel()
	r := newToldRig(t)
	r.seedTold()
	first := r.newsTask("outbox:7")
	first.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, first); err != nil {
		t.Fatalf("confirm the task with its first copy: %v", err)
	}
	r.wantTold("after the first confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)

	// The take-back of that confirmation and its repair were never written. A person approved
	// version 6 since, and the prompt whose turn confirms the task again carried version 6 of the
	// spec and no opening.
	var copied record.DeliveryNews
	if err := json.Unmarshal(first.News, &copied); err != nil {
		t.Fatal(err)
	}
	copied.Items = []record.DeliveryNewsItem{{Issue: toldChild, Audience: record.AudienceHolder, Step: record.DesignStepSpec,
		Artifact: "artifact-spec", From: new(3), To: 6, Generation: 1, TreeGeneration: 1}}
	copied.Openings = nil
	encoded, err := json.Marshal(copied)
	if err != nil {
		t.Fatal(err)
	}
	second := first
	second.News, second.Carried, second.ConfirmedAt = encoded, encoded, r.at.Add(2*time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, second); err != nil {
		t.Fatalf("confirm the task again with its second copy: %v", err)
	}
	r.wantTold("after a second confirmation over the first", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 6}, true)
	r.wantTold("after a second confirmation over the first", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)

	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, second); err != nil {
		t.Fatalf("write the second confirmation again: %v", err)
	}
	r.wantTold("after the second confirmation was written again", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 6}, true)

	takenBack := second
	takenBack.ConfirmedAt, takenBack.Carried = time.Time{}, nil
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, takenBack); err != nil {
		t.Fatalf("take the second confirmation back: %v", err)
	}
	r.wantTold("after the take-back of the second confirmation", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
}
