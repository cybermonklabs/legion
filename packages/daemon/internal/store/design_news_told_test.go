package store

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"strings"
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
	r.issue(toldRoot, "")
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

// told reads issue's audience row as every reader reads it (the SQL function design_news_told_of,
// record.Postgres.TreeTold) as a member of toldRoot's tree: the version of each step it holds with
// every standing credit rendered under toldRoot counted in, whether it owes the opening, and when a
// write of its versions last touched it.
func (r *toldRig) told(issue string, audience record.DesignAudience) (map[record.DesignStep]int, bool, time.Time) {
	r.t.Helper()
	var encoded []byte
	var owed bool
	var at *time.Time
	if err := r.store.Pool().QueryRow(r.ctx, `select versions, opening_owed, told_at from design_news_told_of($3, array[$1]::text[])
		where issue = $1 and audience = $2`, issue, string(audience), toldRoot).Scan(&encoded, &owed, &at); err != nil {
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
// no version of it and whose review history could not say which (record.DeliveryHistoryUnread),
// and the opening to the child's holder, under the opening number the child's row holds as tell
// writes it (0). Each is rendered in the run the rig records its issues in, generation 1 of the
// issue and of its tree's gate root, toldRoot (keptRig.issue). Its prompt was acknowledged
// carrying exactly that news, so the delivery holds it as the copy its confirmation credits
// (supervise.Delivery.Carried).
func (r *toldRig) newsTask(id string) supervise.Delivery {
	r.t.Helper()
	news, err := json.Marshal(record.DeliveryNews{Task: "Review the change.", Items: []record.DeliveryNewsItem{
		{Issue: toldChild, Audience: record.AudienceHolder, Step: record.DesignStepSpec, Artifact: "artifact-spec", From: new(3), To: 5,
			Generation: 1, Root: toldRoot, TreeGeneration: 1},
		{Issue: toldRoot, Audience: record.AudienceArchitect, Step: record.DesignStepCriteria, Artifact: "artifact-checks", To: 2,
			Generation: 1, Root: toldRoot, TreeGeneration: 1,
			HistoryUnread: &record.DeliveryHistoryUnread{Reason: "Dispatch failed 3 reads of the document's review history, the last with: 502 BAD_GATEWAY",
				ReadAt: r.at}},
	}, Openings: []record.DeliveryOpening{{Issue: toldChild, Audience: record.AudienceHolder, Generation: 1, Root: toldRoot, TreeGeneration: 1}}})
	if err != nil {
		r.t.Fatal(err)
	}
	return supervise.Delivery{ID: id, Task: "Review the change.\n\nThe design gate opened.", Phase: phase.Reviewing,
		Generation: 1, QueuedAt: r.at, News: news, Carried: news}
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
// records the delivery's confirmation is the write that makes its credit: each item's told version
// and each opening it carried read as taken, in that write's own transaction, so a confirmation
// rolled back credits nothing, and one that commits cannot have left the told rows behind. Writing
// the task before then — the queue, and every write while it waits — credits nothing. The delivery
// is read back carrying the news it was written with. The write that records that the turn ran
// makes the credit final, and folds it into the told rows at the confirmation's moment.
func TestAConfirmationRaisesWhatItsNewsToldInItsOwnTransaction(t *testing.T) {
	t.Parallel()
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
	if stored.Items[0].HistoryUnread != nil || stored.Items[1].HistoryUnread == nil || !stored.Items[1].HistoryUnread.ReadAt.Equal(r.at) ||
		!strings.Contains(stored.Items[1].HistoryUnread.Reason, "502 BAD_GATEWAY") {
		t.Fatalf("the delivery read back carries the items %+v, want the unread history on the architect's item alone, as written", stored.Items)
	}

	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	refused := errors.New("the transaction that confirms fails after the delivery's write")
	if err := Transact(r.ctx, r.store.Pool(), func(ctx context.Context, tx pgx.Tx) error {
		if err := putDelivery(ctx, tx, r.reviewer.Token, confirmed, r.store.gateRootsOf()); err != nil {
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
	if _, _, at := r.told(toldChild, record.AudienceHolder); !at.Equal(r.at) {
		t.Fatalf("the child's holder row was written at %s before its credit was final, want the seed's moment %s", at, r.at)
	}
	final := confirmed
	final.Ran = true
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, final); err != nil {
		t.Fatalf("make the confirmation's credit final: %v", err)
	}
	r.wantTold("after the credit was made final", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	if _, _, at := r.told(toldChild, record.AudienceHolder); !at.Equal(confirmed.ConfirmedAt) {
		t.Fatalf("the child's holder row rose at %s, want the confirmation's moment %s", at, confirmed.ConfirmedAt)
	}
}

// A raise never lowers: a confirmation whose item tells a version below the one the row holds —
// a later delivery's confirmation raised it first — leaves the row where it stands.
func TestAConfirmationNeverLowersAToldVersion(t *testing.T) {
	t.Parallel()
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
// and the agent never read it, so its credit goes: each item's step reads as the version the row
// held before, unknown where the row knew none, and the opening owed again. The resend's own
// confirmation credits it then.
func TestATakenBackConfirmationPutsBackWhatItRaised(t *testing.T) {
	t.Parallel()
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

// What a taken-back confirmation ends is only its own credit: a step that rose past the item's
// version since — a raise or another credit folded in — keeps what it holds, since the session
// holding the issue took that version from somebody else. Only a fold or the gate settles the
// row's owed opening, so the raise leaves it owed, and the take-back reads it owed again.
func TestATakenBackConfirmationKeepsAVersionRaisedPastWhatItTold(t *testing.T) {
	t.Parallel()
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
	r.tell(toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 6}, true)

	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, task); err != nil {
		t.Fatalf("take the confirmation back: %v", err)
	}
	r.wantTold("after the confirmation was taken back over a later raise", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 6}, true)
}

// A take-back writes no told row, so a write of the same row by another transaction — here the
// fill of the Verification document's version, as the reconcile writes one
// (record.Postgres.FillTold) — neither holds it up nor is lost to it. The fold of a final credit
// does write the row, and computes it from the row it locks, never from a read before that lock
// (design_news_folded): the fill that commits while the fold waits for the row's lock is kept, and
// the fold still takes the spec from the version the row held to the version the credit told. The
// fold is started while the other transaction holds the row, and that transaction commits only
// once Postgres shows the fold waiting on it, so the two always meet in this order.
func TestAFoldKeepsAWriteOfTheSameRowThatCommitsWhileItWaits(t *testing.T) {
	t.Parallel()
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
	r.wantTold("after the confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)

	fill, err := r.store.Pool().BeginTx(r.ctx, pgx.TxOptions{})
	if err != nil {
		t.Fatalf("begin the fill: %v", err)
	}
	defer func() { _ = fill.Rollback(r.ctx) }()
	var fillPID int
	if err := fill.QueryRow(r.ctx, `select pg_backend_pid()`).Scan(&fillPID); err != nil {
		t.Fatalf("read the fill's backend: %v", err)
	}
	if _, err := fill.Exec(r.ctx, `insert into design_news_told (issue, audience, versions, opening_owed, told_at)
		values ($1, 'holder', '{"criteria": 2}', false, $2)
		on conflict (issue, audience) do update set versions = excluded.versions || design_news_told.versions`, toldChild, r.at); err != nil {
		t.Fatalf("fill the child's holder row: %v", err)
	}

	// The take-back and the second confirmation land while the fill still holds the row.
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, task); err != nil {
		t.Fatalf("take the confirmation back while the fill holds the row: %v", err)
	}
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, confirmed); err != nil {
		t.Fatalf("confirm the task again while the fill holds the row: %v", err)
	}

	final := confirmed
	final.Ran = true
	folded := make(chan error, 1)
	go func() { folded <- r.store.PutDelivery(r.ctx, r.reviewer.Token, final) }()
	waitedOn := false
	for deadline := time.Now().Add(30 * time.Second); !waitedOn && time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if err := r.store.Pool().QueryRow(r.ctx, `select exists (select 1 from pg_stat_activity where $1::int = any(pg_blocking_pids(pid)))`,
			fillPID).Scan(&waitedOn); err != nil {
			t.Fatalf("read whether the fold waits on the fill: %v", err)
		}
	}
	if !waitedOn {
		t.Fatal("the fold never waited on the fill's lock of the child's holder row, so the two did not meet")
	}
	if err := fill.Commit(r.ctx); err != nil {
		t.Fatalf("commit the fill: %v", err)
	}
	if err := <-folded; err != nil {
		t.Fatalf("make the credit final: %v", err)
	}
	if err := r.store.RetireDelivery(r.ctx, r.reviewer, final.ID); err != nil {
		t.Fatalf("retire the task: %v", err)
	}
	r.wantTold("after a fold that waited on a fill of the same row", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}, false)
}

// A delivery retired without a confirmation — its phase moved on, it was replaced, or it was left
// with nothing to say — was never read, so it raises nothing and clears no opening: the news stays
// owed, and the next start or the reconcile tells it.
func TestARetirementRaisesNothing(t *testing.T) {
	t.Parallel()
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
// decision. That write carries the same credit, so it credits nothing a second time and clears
// nothing: an opening the gate owes the holder again since the first write (a close and a reopening
// settle it, design_news_opening_settled) stays owed.
func TestASecondConfirmedWriteRaisesNothingAgain(t *testing.T) {
	t.Parallel()
	r := newToldRig(t)
	r.seedTold()
	confirmed := r.newsTask("outbox:7")
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after the confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	r.exec(`select design_news_opening_settled($1, 'holder', true)`, toldChild)

	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("write the confirmed task again: %v", err)
	}
	r.wantTold("after the confirmed task was written again", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, true)
	r.wantTold("after the confirmed task was written again", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5, record.DesignStepCriteria: 2}, false)
}

// The confirmation raises the copy of the news the prompt its turn answers carried
// (supervise.Delivery.Carried), never the news the delivery holds now: a send since that prompt —
// a restart's, or a resend Oh My Pi answered busy — rendered the news again and took the opening
// on, and the turn that starts is the earlier prompt's, which said nothing of it. So the item the
// prompt carried rises, and the opening stays owed. A delivery whose copy is empty — no prompt of
// it was ever acknowledged — raises nothing at all.
func TestAConfirmationRaisesWhatItsPromptCarriedAndNotWhatTheTaskSaysNow(t *testing.T) {
	t.Parallel()
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	var withoutOpening record.DeliveryNews
	if err := json.Unmarshal(task.News, &withoutOpening); err != nil {
		t.Fatal(err)
	}
	withoutOpening.Openings = nil
	carried, err := json.Marshal(withoutOpening)
	if err != nil {
		t.Fatal(err)
	}
	task.Carried = carried
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, task); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after a confirmation whose prompt carried no opening", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, true)

	r.seedTold()
	unacknowledged := r.newsTask("outbox:8")
	unacknowledged.Carried = nil
	if err := r.store.RetireDelivery(r.ctx, r.reviewer, confirmed.ID); err != nil {
		t.Fatalf("retire the first task: %v", err)
	}
	r.seedTold()
	unacknowledged.ConfirmedAt = r.at.Add(2 * time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, unacknowledged); err != nil {
		t.Fatalf("confirm a task no acknowledged prompt carried: %v", err)
	}
	r.wantTold("after a confirmation of a task no acknowledged prompt carried", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
}

// An opening is named by the number its told row held when it became owed (opening_seq), and a
// prompt carries that number (record.DeliveryOpening.Seq). The gate closes and opens again before
// the turn of a prompt that carried the opening: the close and the reopening each move the number,
// so the confirmation of that prompt clears nothing of the new opening, which stays owed, and the
// take-back of that confirmation owes nothing the reopening has not owed already. A prompt that
// carried the opening under the row's own number clears it, and its take-back owes it again.
func TestAConfirmationClearsOnlyTheOpeningItsPromptCarried(t *testing.T) {
	t.Parallel()
	r := newToldRig(t)
	r.seedTold()
	task := r.newsTask("outbox:7")
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, task); err != nil {
		t.Fatalf("queue the task: %v", err)
	}
	// The gate closes and opens again while the reviewer is still owed news: the close settles the
	// row's opening, and the reopening owes it a new one.
	r.exec(`select design_news_opening_settled($1, 'holder', false)`, toldChild)
	r.exec(`select design_news_opening_settled($1, 'holder', true)`, toldChild)
	confirmed := task
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after the confirmation of a prompt that carried the earlier opening", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, true)

	// The take-back of that confirmation owes nothing more than the reopening does.
	r.exec(`select design_news_opening_settled($1, 'holder', false)`, toldChild)
	retaken := task
	retaken.ID = "outbox:7-1"
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, retaken); err != nil {
		t.Fatalf("take the confirmation back: %v", err)
	}
	r.wantTold("after the take-back of a confirmation of an earlier opening", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, false)

	// A prompt carrying the row's own opening number clears that opening, and its take-back owes
	// it again.
	var seq int
	if err := r.store.Pool().QueryRow(r.ctx, `select opening_seq from design_news_told where issue = $1 and audience = 'holder'`,
		toldChild).Scan(&seq); err != nil {
		t.Fatalf("read the child's opening number: %v", err)
	}
	r.exec(`select design_news_opening_settled($1, 'holder', true)`, toldChild)
	var current record.DeliveryNews
	if err := json.Unmarshal(task.News, &current); err != nil {
		t.Fatal(err)
	}
	current.Openings[0].Seq = seq + 1
	encoded, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	retaken.News, retaken.Carried = encoded, encoded
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, retaken); err != nil {
		t.Fatalf("write the task acknowledged under the current opening: %v", err)
	}
	reconfirmed := retaken
	reconfirmed.ConfirmedAt = r.at.Add(2 * time.Minute)
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, reconfirmed); err != nil {
		t.Fatalf("confirm the task again: %v", err)
	}
	r.wantTold("after the confirmation of a prompt that carried the current opening", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, retaken); err != nil {
		t.Fatalf("take the confirmation back: %v", err)
	}
	r.wantTold("after the take-back of the confirmation of the current opening", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
}

// A confirmed delivery retired while its credit is not final — a newer task took its place while
// its turn ran, or its turn ended with no result the daemon was waiting on — ends that credit with
// its row, in the retirement's own transaction: the result of that turn, a provider error among
// them, now answers a task the claim no longer holds. A confirmed delivery whose turn's result said
// the turn ran (supervise.Delivery.Ran) folded its credit into the told rows when that was written,
// and its retirement takes nothing back.
func TestARetirementOfAConfirmedTaskGivesBackWhatItsTurnHasNotMadeFinal(t *testing.T) {
	t.Parallel()
	r := newToldRig(t)
	r.seedTold()
	confirmed := r.newsTask("outbox:7")
	confirmed.ConfirmedAt = r.at.Add(time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, confirmed); err != nil {
		t.Fatalf("confirm the task: %v", err)
	}
	r.wantTold("after the confirmation", toldChild, record.AudienceHolder, map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
	if err := r.store.RetireDelivery(r.ctx, r.reviewer, confirmed.ID); err != nil {
		t.Fatalf("retire the task: %v", err)
	}
	r.wantTold("after the retirement of a confirmed task still holding its copy", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 3}, true)
	r.wantTold("after the retirement of a confirmed task still holding its copy", toldRoot, record.AudienceArchitect,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)

	final := r.newsTask("outbox:8")
	final.ConfirmedAt = r.at.Add(2 * time.Minute)
	if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, final); err != nil {
		t.Fatalf("confirm the second task: %v", err)
	}
	final.Ran = true
	if err := r.store.PutDelivery(r.ctx, r.reviewer.Token, final); err != nil {
		t.Fatalf("make the second task's credit final: %v", err)
	}
	if err := r.store.RetireDelivery(r.ctx, r.reviewer, final.ID); err != nil {
		t.Fatalf("retire the second task: %v", err)
	}
	r.wantTold("after the retirement of a confirmed task whose turn's result came", toldChild, record.AudienceHolder,
		map[record.DesignStep]int{record.DesignStepSpec: 5}, false)
}
