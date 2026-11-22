package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/reviewround"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// roundNewsHead is the commit the reviewer's own deciding review read in these cases
// (record.PhaseRow.ReviewedHead): a head as a Legion footer names one.
const roundNewsHead = "89abcdef0123456789abcdef0123456789abcdef"

// wantRoundLast fails the test unless task is own (where own is not empty), a blank line, the news
// of the spec from version 4 to 5, and then round: the two sentences of the reviewer's round once,
// and last, where the reviewer's prompt reads them.
func wantRoundLast(t *testing.T, what, task, own, round string) {
	t.Helper()
	body, sentences, named := reviewround.Split(task)
	if !named || sentences != round || strings.Count(task, reviewround.CommitLabel) != 1 || strings.Count(task, reviewround.EntryLabel) != 1 {
		t.Fatalf("%s = %q, want it to end with the round's two sentences %q, once", what, task, round)
	}
	if own != "" && !strings.HasPrefix(body, own+"\n\n") {
		t.Fatalf("%s = %q, want the start's own text, then a blank line, before the news", what, task)
	}
	if !strings.Contains(body, "version 5, since version 4, which your phase last took") || !strings.Contains(body, `"Design" changed`) {
		t.Fatalf("%s = %q, want the spec's news from version 4 to 5 before the round's two sentences", what, task)
	}
}

// A reviewer's task ends with the daemon's two sentences of its review round, which its prompt
// reads as the task's last two: the commit its own newest deciding review read and the entry of
// its round (reviewround.Sentences). A reviewer's start that carries news of the approved design,
// from a task the queue named (record.SuperviseRequest.RoundNamed), is rendered with the news
// between the start's own text and those two sentences, at the start and at the send of the held
// task again (renderNews), so the reviewer still reads its commit and its entry.
func TestAReviewersStartThatCarriesNewsKeepsTheRoundsTwoSentencesLast(t *testing.T) {
	r := newNewsReconcileRig(t)
	reviewed := r.member(reconcileChild, reconcileRoot, phase.Reviewing)
	r.told(reconcileChild, record.AudienceHolder, 4)
	r.record(record.VersionPair{ArtifactID: reconcileDoc, From: 4, To: 5}, "")
	token := r.holder(reconcileChild, claim.RoleReviewer, supervise.StateIdle, "", 1)
	const own = "Review the change. Issue: LEGION-601. Phase: reviewing."
	round := reviewround.Sentences(roundNewsHead, 1)

	started, err := r.runner.newsForStart(r.ctx, reviewed, record.SuperviseRequest{Op: "start", Tree: reconcileRoot,
		Role: claim.RoleReviewer, Phase: phase.Reviewing, Generation: 1, Entry: 1, Task: own + round, RoundNamed: true}, token)
	if err != nil {
		t.Fatalf("compute the news of the reviewer's start: %v", err)
	}
	if !started.roundNamed || len(started.news) == 0 {
		t.Fatalf("the reviewer's start carries %+v, want news, and its task marked as naming its round", started)
	}
	wantRoundLast(t, "the reviewer's start task", started.task, own, round)

	sender := &workflowRuntime{pool: r.pool, records: r.records, gatedSteps: config.DesignGateRootIssues.Steps(),
		dispatchProject: "LEGION", log: quietLogger()}
	reviewer := supervise.Claim{Token: token, Project: "legion", Tree: reconcileRoot, Issue: reconcileChild, Role: claim.RoleReviewer}
	resent, _, err := sender.designNews(r.ctx, reviewer, supervise.Delivery{ID: "outbox:3", Task: started.task, Phase: phase.Reviewing,
		Generation: 1, Entry: 1, News: started.news, RoundNamed: true})
	if err != nil {
		t.Fatalf("send the reviewer's held task again: %v", err)
	}
	wantRoundLast(t, "the reviewer's task sent again", resent, own, round)
}

// A start of the reviewer queued for news alone carries no task, so no queue named its round, and
// the claim's send would give it the two sentences naming no commit and no entry, though the record
// holds both. The start executor names them from the record instead (workflow.ReviewerTask): the
// commit the reviewer's row holds, or `none` where it holds none, and the entry the row stands at,
// after the news, and the delivery says the task names its round, so its send adds no second pair
// (supervise's Delivery.message).
func TestAReviewersNewsOnlyTaskNamesTheRoundTheRecordHolds(t *testing.T) {
	for _, tc := range []struct {
		name     string
		reviewed string
	}{
		{name: "a reviewed commit", reviewed: roundNewsHead},
		{name: "no reviewed commit", reviewed: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newNewsReconcileRig(t)
			r.told(reconcileRoot, record.AudienceHolder, 4)
			token := r.holder(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 3)
			r.inTx(func(tx pgx.Tx) error {
				return r.records.PutPhase(r.ctx, tx, record.PhaseRow{Issue: reconcileRoot, Role: claim.RoleReviewer, Claim: token, Entry: 3,
					EntryTask: "Review the change.", EntryRaisedAt: time.Now().UTC(), ReviewedHead: tc.reviewed})
			})
			if err := r.reconcile(0); err != nil {
				t.Fatalf("reconcile the tree's news: %v", err)
			}
			starts := r.newsStarts()
			if got := startsTo(starts); len(got) != 1 || got[0] != reconcileRoot+"/reviewer@reviewing" {
				t.Fatalf("the news-only starts = %v, want the reviewer's", got)
			}

			machine := r.machine(reconcileRoot, claim.RoleReviewer, supervise.StateIdle, "", 3)
			if err := r.runner.execute(r.ctx, starts[0].row); err != nil {
				t.Fatalf("run the news-only start: %v", err)
			}
			task, _ := pendingNews(t, machine)
			if pending := machine.Claim().Pending; !pending.NewsOnly || !pending.RoundNamed {
				t.Fatalf("the reviewer holds %+v, want a task of news alone that says it names its round", pending)
			}
			wantRoundLast(t, "the reviewer's news-only task", task, "", reviewround.Sentences(tc.reviewed, 3))
		})
	}
}
