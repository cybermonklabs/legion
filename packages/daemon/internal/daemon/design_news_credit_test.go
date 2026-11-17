package daemon

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// The cases here are two traces. In each, a delivery's confirmation credited news its turn's
// result has not yet made final, and something then built on that credit: a start that computed
// its news from the row the credit raised, and the reconcile's raise past it. The credit is then
// given back. Since a credit is the delivery's own and a raise is a credit counted from the
// version its pair names, the give-back ends exactly that credit, and whatever was counted from
// the version it gave reaches nothing: the news of the pair before it is told, never lost.

// creditRig is the reconcile rig with its child in implementing, its holder's row at version 3 of
// the spec, its implementer's claim, and the daemon's claim store on the rig's pool.
type creditRig struct {
	*newsReconcileRig
	store *store.Store
	token claim.Token
	child record.Issue
}

// newCreditRig is a creditRig on a database of its own.
func newCreditRig(t *testing.T) *creditRig {
	t.Helper()
	r := &creditRig{newsReconcileRig: newNewsReconcileRig(t)}
	r.child = r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileChild, record.AudienceHolder, 3)
	// The root stands in reviewing, and its holder took the version the gate stands at, so the
	// child's holder is the one row the tree owes anything.
	r.told(reconcileRoot, record.AudienceHolder, 5)
	r.token = r.holder(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)
	r.store = outboxTreeStore(t, r.pool)
	return r
}

// claim is the implementer's claim as the store holds it.
func (r *creditRig) claim() supervise.Claim {
	r.t.Helper()
	claims, err := r.store.Claims(r.ctx)
	if err != nil {
		r.t.Fatalf("read the claims: %v", err)
	}
	return storedOf(r.t, claims, r.token)
}

// confirmedTask writes the implementer's task id, whose prompt carried version to of the spec
// since version from, as confirmed by its turn, its result not yet come: the delivery's credit.
func (r *creditRig) confirmedTask(id string, from, to int) supervise.Delivery {
	r.t.Helper()
	news, err := json.Marshal(record.DeliveryNews{Task: "Implement the plan.", Items: []record.DeliveryNewsItem{
		newsItem(reconcileChild, record.AudienceHolder, record.DesignStepSpec, reconcileDoc, from, to)}})
	if err != nil {
		r.t.Fatal(err)
	}
	at := time.Now().UTC()
	d := supervise.Delivery{ID: id, Task: "Implement the plan.\n\nThe approved design changed.", Phase: phase.Implementing,
		Generation: 1, QueuedAt: at, News: news, Carried: news, ConfirmedAt: at}
	if err := r.store.PutDelivery(r.ctx, r.token, d); err != nil {
		r.t.Fatalf("confirm the task %s: %v", id, err)
	}
	return d
}

// took is the version of the spec the child's holder took, as every reader reads it.
func (r *creditRig) took() int {
	r.t.Helper()
	took, known := r.toldOf(reconcileChild, record.AudienceHolder).Took(record.DesignStepSpec)
	if !known {
		r.t.Fatal("the child's holder row knows no version of the spec")
	}
	return took
}

// owed is what record.OwedNews owes the child's holder now.
func (r *creditRig) owed() []record.NewsItem {
	r.t.Helper()
	var items []record.NewsItem
	r.inTx(func(tx pgx.Tx) error {
		facts, err := record.TreeNewsFacts(r.ctx, r.records, tx, config.DesignGateRootIssues.Steps(), reconcileRoot)
		if err != nil {
			return err
		}
		member, _ := record.OwedNews(facts).Member(reconcileChild)
		items = member.Items
		return nil
	})
	return items
}

// Trace (a), a stale From. Task D1 told the implementer version 4 of the spec since version 3, its
// turn confirmed it, and the turn's result has not come. The gate stands approved at version 5, so
// a start for the same claim computes its news from the row D1's credit raised: version 5 since
// version 4. A newer task then takes D1's place, which gives D1's credit back. The start's task is
// sent after that, and its send renders it again from the record: it tells version 5 since version
// 3, the version the holder took now, and the news it carries names that pair, so its credit
// reaches the row as it stands. Carried as the start computed it, the task would tell the change
// list of 4 to 5 alone, and nobody would be told what changed from 3 to 4.
func TestAStartComputedFromAnUnfinalCreditTellsThePairBeforeItOnceThatCreditIsGivenBack(t *testing.T) {
	r := newCreditRig(t)
	for _, pair := range []record.VersionPair{{ArtifactID: reconcileDoc, From: 3, To: 4}, {ArtifactID: reconcileDoc, From: 4, To: 5},
		{ArtifactID: reconcileDoc, From: 3, To: 5}} {
		r.record(pair, "")
	}
	first := r.confirmedTask("outbox:1", 3, 4)
	if took := r.took(); took != 4 {
		t.Fatalf("the child's holder took version %d after D1's confirmation, want 4: the case's premise", took)
	}

	started, err := r.runner.newsForStart(r.ctx, r.child, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
		Role: claim.RoleImplementer, Phase: phase.Implementing, Generation: 1, News: true}, r.token)
	if err != nil {
		t.Fatalf("compute the start's news: %v", err)
	}
	var computed record.DeliveryNews
	if err := json.Unmarshal(started.news, &computed); err != nil {
		t.Fatalf("decode the start's news %s: %v", started.news, err)
	}
	if len(computed.Items) != 1 || computed.Items[0].From == nil || *computed.Items[0].From != 4 || computed.Items[0].To != 5 {
		t.Fatalf("the start carries %+v, want version 5 since version 4: the case's premise", computed.Items)
	}

	if err := r.store.RetireDelivery(r.ctx, r.claim(), first.ID); err != nil {
		t.Fatalf("retire D1 before its turn's result: %v", err)
	}
	if took := r.took(); took != 3 {
		t.Fatalf("the child's holder took version %d after D1 gave its credit back, want 3", took)
	}

	sender := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(),
		dispatchProject: "LEGION", log: quietLogger()}
	task, news, err := sender.designNews(r.ctx, r.claim(), supervise.Delivery{ID: "outbox:2", Task: started.task, Phase: phase.Implementing,
		Generation: 1, News: started.news, NewsOnly: true})
	if err != nil {
		t.Fatalf("render the start's task at its send: %v", err)
	}
	var sent record.DeliveryNews
	if err := json.Unmarshal(news, &sent); err != nil {
		t.Fatalf("decode the news the sent task carries %s: %v", news, err)
	}
	if len(sent.Items) != 1 || sent.Items[0].From == nil || *sent.Items[0].From != 3 || sent.Items[0].To != 5 {
		t.Fatalf("the sent task carries %+v, want version 5 since version 3, the version its holder took now", sent.Items)
	}
	if !strings.Contains(task, "version 5, since version 3") || strings.Contains(task, "since version 4") {
		t.Fatalf("the sent task = %q, want version 5 told since version 3 and no word of version 4 as taken", task)
	}
}

// Trace (b), a raise past an unfinal credit. Task D1's confirmation credited version 4 of the spec
// since version 3, and its turn's result has not come. Version 5 changed decision blocks alone
// since version 4, so the reconcile raises the holder's row past that pair and sends nothing. D1 is
// then retired before its result: its credit goes, and the raise, counted from the version D1 gave
// the row, reaches nothing. The holder reads as having taken version 3 again, and is owed version 5
// since version 3, whose change list says what changed from 3 to 4.
func TestARaisePastAnUnfinalCreditGoesWithThatCredit(t *testing.T) {
	r := newCreditRig(t)
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		if pair.From == 4 && pair.To == 5 {
			return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}, nil
		}
		return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To,
			Sections: []dispatch.SectionChange{{Heading: "Design", Change: dispatch.ChangeChanged}}}, nil
	}
	first := r.confirmedTask("outbox:1", 3, 4)
	if err := r.reconcile(0); err != nil {
		t.Fatalf("reconcile the tree's news: %v", err)
	}
	if took := r.took(); took != 5 {
		t.Fatalf("the child's holder took version %d after the reconcile, want 5: the raise past a decision-only pair, the case's premise", took)
	}

	if err := r.store.RetireDelivery(r.ctx, r.claim(), first.ID); err != nil {
		t.Fatalf("retire D1 before its turn's result: %v", err)
	}
	if took := r.took(); took != 3 {
		t.Fatalf("the child's holder took version %d after D1 gave its credit back, want 3: the raise counted from D1's version goes with it", took)
	}
	if owed := r.owed(); len(owed) != 1 || owed[0].From == nil || *owed[0].From != 3 || owed[0].To != 5 {
		t.Fatalf("the child's holder is owed %+v, want version 5 since version 3", owed)
	}
}
