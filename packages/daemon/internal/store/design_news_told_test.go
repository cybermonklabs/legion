package store

import (
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// toldRig is a store holding one tree, its root and a child the root's reviewer works, the told
// rows of both, and the reviewer's claim, whose deliveries carry news of the approved design.
type toldRig struct {
	*keptRig
	reviewer supervise.Claim
}

const (
	toldRoot  = "LEGION-400"
	toldChild = "LEGION-401"
)

func newToldRig(t *testing.T) *toldRig {
	t.Helper()
	r := &toldRig{keptRig: newKeptRig(t)}
	r.issue(toldRoot, toldRoot)
	r.issue(toldChild, toldRoot)
	token, err := claim.NewToken("legion", toldChild, claim.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	r.reviewer = supervise.Claim{Token: token, Project: "legion", Tree: toldRoot, Issue: toldChild, Role: claim.RoleReviewer,
		Generation: 1, State: supervise.StateReady}
	if err := r.store.PutClaim(r.ctx, r.reviewer); err != nil {
		t.Fatalf("put the reviewer's claim: %v", err)
	}
	return r
}

// tell writes issue's audience row as holding versions, owing the opening or not.
func (r *toldRig) tell(issue string, audience record.DesignAudience, versions map[record.DesignStep]int, openingOwed bool) {
	r.t.Helper()
	encoded, err := json.Marshal(versions)
	if err != nil {
		r.t.Fatal(err)
	}
	r.exec(`insert into design_news_told (issue, audience, versions, opening_owed, told_at) values ($1, $2, $3, $4, $5)
		on conflict (issue, audience) do update set versions = excluded.versions, opening_owed = excluded.opening_owed`,
		issue, string(audience), encoded, openingOwed, r.at)
}

// told reads issue's audience row: the version of each step it holds, whether it owes the opening,
// and when it last rose.
func (r *toldRig) told(issue string, audience record.DesignAudience) (map[record.DesignStep]int, bool, time.Time) {
	r.t.Helper()
	var encoded []byte
	var owed bool
	var at *time.Time
	if err := r.store.Pool().QueryRow(r.ctx, `select versions, opening_owed, told_at from design_news_told
		where issue = $1 and audience = $2`, issue, string(audience)).Scan(&encoded, &owed, &at); err != nil {
		r.t.Fatalf("read the %s row of %s: %v", audience, issue, err)
	}
	var versions map[record.DesignStep]int
	if err := json.Unmarshal(encoded, &versions); err != nil {
		r.t.Fatal(err)
	}
	if at == nil {
		return versions, owed, time.Time{}
	}
	return versions, owed, at.UTC()
}

// wantTold fails the test unless issue's audience row holds versions and owes the opening as owed.
func (r *toldRig) wantTold(why string, issue string, audience record.DesignAudience, versions map[record.DesignStep]int, owed bool) {
	r.t.Helper()
	got, gotOwed, _ := r.told(issue, audience)
	if !maps.Equal(got, versions) || gotOwed != owed {
		r.t.Fatalf("%s: the %s row of %s holds %v and owes the opening %t, want %v and %t", why, audience, issue, got, gotOwed, versions, owed)
	}
}

// newsTask is the reviewer's task carrying news: version 5 of the spec to the child's holder, which
// took version 3, version 2 of the Verification document to the root's architect, whose row knows
// no version of it, and the opening to the child's holder.
func (r *toldRig) newsTask(id string) supervise.Delivery {
	r.t.Helper()
	news, err := json.Marshal(record.DeliveryNews{Task: "Review the change.", Items: []record.DeliveryNewsItem{
		{Issue: toldChild, Audience: record.AudienceHolder, Step: record.DesignStepSpec, Artifact: "artifact-spec", From: new(3), To: 5},
		{Issue: toldRoot, Audience: record.AudienceArchitect, Step: record.DesignStepCriteria, Artifact: "artifact-checks", To: 2},
	}, Openings: []record.DeliveryOpening{{Issue: toldChild, Audience: record.AudienceHolder}}})
	if err != nil {
		r.t.Fatal(err)
	}
	return supervise.Delivery{ID: id, Task: "Review the change.\n\nThe design gate opened.", Phase: phase.Reviewing,
		Generation: 1, QueuedAt: r.at, News: news}
}

// seedTold is what the two audiences took before the news: the child's holder spec version 3,
// owing the opening, and the root's architect spec version 5 and no version of the Verification
// document.
func (r *toldRig) seedTold() {
	r.t.Helper()
	r.tell(toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
	r.tell(toldRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
}

// A turn that starts carrying news is what makes its audience have taken it, so the write that
// records the delivery's confirmation is the write that raises each item's told version and
// clears each opening it carried, in that write's own transaction: a confirmation rolled back
// raises nothing, and one that commits cannot have left the told rows behind. Writing the task
// before then — the queue, and every write while it waits — raises nothing. The delivery is read
// back carrying the news it was written with.
func TestAConfirmationRaisesWhatItsNewsToldInItsOwnTransaction(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, task); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	r.wantTold("after the queue", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3}, true)

	claims, err := r.store.Claims(r.ctx)
	if err != nil {
		t.Fatalf("read the claims back: %v", err)
	}
	var stored record.DeliveryNews
	for _, c := range claims {
		if c.Token == r.reviewer.Token && c.Pending != nil {
			if err := json.Unmarshal(c.Pending.News, &stored); err != nil {
				t.Fatalf("decode the news the stored delivery carries: %v", err)
			}
		}
	}
	if len(stored.Items) != 2 || len(stored.Openings) != 1 || stored.Task != "Review the change." {
		t.Fatalf("the delivery read back carries %+v, want the news it was written with", stored)
	}

	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	refused := errors.New("the transaction that confirms fails after the delivery's write")
	if err := r.store.Tx(r.ctx, func(tx pgx.Tx) error {
		if err := putDelivery(r.ctx, tx, r.reviewer.Token, confirmed); err != nil {
			return err
		}
		return refused
	}); !errors.Is(err, refused) {
		t.Fatalf("the failed confirmation answered %v, want %v", err, refused)
	}
	r.wantTold("after a confirmation its transaction rolled back", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)

	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after the confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	r.wantTold("after the confirmation", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}, false)
	if _, _, at := r.told(toldChild, record.AudienceHolder); !at.Equal(confirmed.ConfirmedAt) {
		t.Fatalf("the child's holder row rose at %s, want the confirmation's moment %s", at, confirmed.ConfirmedAt)
	}
}

// A raise never lowers: a confirmation whose item tells a version below the one the row holds —
// a later delivery's confirmation raised it first — leaves the row where it stands.
func TestAConfirmationNeverLowersAToldVersion(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	r.tell(toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 6}, true)
	confirmed := r.newsTask("outbox:7")
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after a confirmation of version 5 over a row at 6", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 6}, false)
}

// A confirmation the machine takes back — a prompt refused busy after a turn of the agent's own
// confirmed it, a late refusal, a turn its process died in — writes the delivery back unconfirmed,
// and the agent never read it, so what the confirmation raised goes back: each item's step to the
// version the row held before, forgotten where the row knew none, and the opening owed again. The
// resend's own confirmation raises it then.
func TestATakenBackConfirmationPutsBackWhatItRaised(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	for _, write := range []supervise.Delivery{task, confirmed} {
		if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, write); err != nil {
			t.Fatalf("write the task: %v", err)
		}
	}

	retaken := task
	retaken.ID = "outbox:7-1"
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, retaken); err != nil {
		t.Fatalf("take the confirmation back: %v", err)
	}
	r.wantTold("after the confirmation was taken back", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
	r.wantTold("after the confirmation was taken back", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)

	// The task written again unconfirmed, as every write while it waits is, puts nothing back a
	// second time: the row it replaces holds no confirmation.
	r.tell(toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, retaken); err != nil {
		t.Fatalf("write the task again: %v", err)
	}
	r.wantTold("after an unconfirmed write over no confirmation", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
}

// What a taken-back confirmation puts back is only what it raised: a step that rose past the
// item's version since — a later delivery's confirmation raised it — keeps what it holds, since
// the session holding the issue took that version from somebody else.
func TestATakenBackConfirmationKeepsAVersionRaisedPastWhatItTold(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	for _, write := range []supervise.Delivery{task, confirmed} {
		if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, write); err != nil {
			t.Fatalf("write the task: %v", err)
		}
	}
	r.tell(toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 6}, false)

	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, task); err != nil {
		t.Fatalf("take the confirmation back: %v", err)
	}
	r.wantTold("after the confirmation was taken back over a later raise", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 6}, true)
}

// A delivery retired without a confirmation — its phase moved on, it was replaced, or it was left
// with nothing to say — was never read, so it raises nothing and clears no opening: the news stays
// owed, and the next start or the reconcile tells it.
func TestARetirementRaisesNothing(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, task); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	if err := r.store.RetireDelivery(r.ctx, r.reviewer, task.ID); err != nil {
		t.Fatalf("retire the task: %v", err)
	}
	r.wantTold("after the retirement", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
	r.wantTold("after the retirement", toldRoot, record.AudienceArchitect, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
}

// A confirmed delivery is written again whenever the machine owed a write and makes it at its next
// decision. That write records no confirmation — the row already holds it — so it raises nothing
// and clears nothing: an opening the gate owes the holder again since the first write stays owed.
func TestASecondConfirmedWriteRaisesNothingAgain(t *testing.T) {
	r := newToldRig(t)
	r.seedTold()
	confirmed := r.newsTask("outbox:7")
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after the confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	r.exec(`update design_news_told set opening_owed = true where issue = $1 and audience = 'holder'`, toldChild)

	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("write the confirmed task again: %v", err)
	}
	r.wantTold("after the confirmed task was written again", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, true)
	r.wantTold("after the confirmed task was written again", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}, false)
}
