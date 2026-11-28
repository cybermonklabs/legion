package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/dispatch"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/testpg/testdb"
	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// newsSettleMatch is the statement of the settlement of a tree's news (settleNews) that no other
// transaction of its reconcile sends: the enqueue of a news-only start.
const newsSettleMatch = "insert into outbox (kind, issue, payload, attempts, next_at, last_error, lease_token"

// The two lines of a settlement of a tree's news (settleNews).
const (
	newsStartQueuedLine = "outbox design_changes queued a news-only start"
	newsSettledLine     = "outbox design_changes settled the news its tree is owed"
)

// newsSettleRig is the reconcile of TestADecisionOnlyChangeRaisesWhatEachHolderTookAndSendsNothing's
// tree, its outbox run through a faulted pool: the settlement raises what the root's holder and
// architect took and queues one news-only start, to the child's holder owed the opening.
type newsSettleRig struct {
	news  *newsReconcileRig
	fault *testdb.CommitFault
	logs  *syncBuffer
}

func newNewsSettleRig(t *testing.T) *newsSettleRig {
	r := newNewsReconcileRig(t)
	r.member(reconcileRoot, "", phase.Reviewing)
	r.member(reconcileChild, reconcileRoot, phase.Implementing)
	r.told(reconcileRoot, record.AudienceHolder, 4)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.told(reconcileRoot, record.AudienceArchitect, 4)
	r.owesOpening(reconcileChild, record.AudienceHolder)
	r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 1)
	r.holder(reconcileChild, claim.RoleImplementer, supervise.StateIdle, "", 1)
	r.holder(reconcileRoot, claim.RoleArchitect, supervise.StateWorking, "", 0)
	r.client.answer = func(pair record.VersionPair) (dispatch.ArtifactChanges, error) {
		return dispatch.ArtifactChanges{ArtifactID: pair.ArtifactID, From: pair.From, To: pair.To, DecisionOnly: true}, nil
	}
	faulted, fault := faultedPoolOf(t, r.pool)
	logs, log := watchedLog()
	r.runner.pool, r.runner.log = faulted, log
	return &newsSettleRig{news: r, fault: fault, logs: logs}
}

func (r *newsSettleRig) work() map[string]int {
	return map[string]int{"start lines": count(r.logs, newsStartQueuedLine), "settlement lines": count(r.logs, newsSettledLine)}
}

// newsSettleOwner is the settlement of a tree's news, whose lines saying what it queued and raised
// are hooks of its commit (settleNews).
func newsSettleOwner(t *testing.T) *ownerRig {
	r := newNewsSettleRig(t)
	return &ownerRig{
		fault:   r.fault,
		run:     func(*testing.T) error { return r.news.reconcile(0) },
		changed: func(*testing.T) bool { return len(r.news.newsStarts()) == 1 },
		work:    func(*testing.T) map[string]int { return r.work() },
	}
}

// A settlement whose outcome the store cannot say logs nothing yet. Once the store says it
// committed, its two lines follow, once; the next reconcile finds the start queued and the rows
// raised, and logs neither.
func newsSettleUnknown(t *testing.T) {
	r := newNewsSettleRig(t)
	fired := r.fault.Cut(newsSettleMatch, testdb.CutAfterCommitRefusing)
	if err := r.news.reconcile(0); !errors.Is(err, store.ErrCommitOutcomeUnknown) {
		t.Errorf("the reconcile whose settlement's outcome the store cannot say = %v, want %v", err, store.ErrCommitOutcomeUnknown)
	}
	testdb.Met(t, fired)
	if work := r.work(); work["start lines"] != 0 || work["settlement lines"] != 0 {
		t.Fatalf("before the store answers: %v, want none", work)
	}
	r.fault.Refuse(false)
	testwait.WithinPolling(t, testwait.Bounded(t, 10*time.Second, time.Second), testwait.Poll,
		"the settlement's lines once the store says it committed", func() bool {
			work := r.work()
			return work["start lines"] == 1 && work["settlement lines"] == 1
		})
	if err := r.news.reconcile(0); err != nil {
		t.Fatalf("the next reconcile = %v, want nil", err)
	}
	if work := r.work(); work["start lines"] != 1 || work["settlement lines"] != 1 {
		t.Errorf("after the next reconcile: %v, want each once", work)
	}
	if starts := r.news.newsStarts(); len(starts) != 1 {
		t.Errorf("news-only starts after the next reconcile = %d, want the 1 that committed", len(starts))
	}
}
