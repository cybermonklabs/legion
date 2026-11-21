package store

import (
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// Whether a delivery is a task of news alone (supervise.Delivery.NewsOnly) is a column of the
// delivery's own (pending_task_deliveries.news_only), written and read beside the news it carries
// and never inside it. The news here is built by a writer that sets no mark of any kind — the news
// a send renders again, or any later writer of record.DeliveryNews — so the mark can only come back
// from the column. Both readers of a claim's pending task read it back — the claim store's restore
// (claimSelect) and a role run's (record's readRoleClaims) — so a task of news alone restored after
// a restart is still no task of its phase to the two rules every reader of a pending task asks
// (supervise.Delivery.StillWorked, supervise.HoldsWork), and a phase task carrying the same news
// is that phase's task.
func TestATaskOfNewsAloneIsReadBackMarkedSoByBothReadersOfAClaimsTask(t *testing.T) {
	for _, tc := range []struct {
		name     string
		newsOnly bool
	}{
		{name: "a task of news alone", newsOnly: true},
		{name: "a phase task carrying news"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newToldRig(t)
			task := r.newsTask("outbox:7")
			news, err := json.Marshal(record.DeliveryNews{Items: []record.DeliveryNewsItem{
				{Issue: toldChild, Audience: record.AudienceHolder, Step: record.DesignStepSpec, Artifact: "artifact-spec", From: new(3), To: 5}}})
			if err != nil {
				t.Fatal(err)
			}
			task.News, task.NewsOnly = news, tc.newsOnly
			if err := r.store.PutClaimAndDelivery(r.ctx, r.reviewer, task); err != nil {
				t.Fatalf("queue the task: %v", err)
			}

			claims, err := r.store.Claims(r.ctx)
			if err != nil {
				t.Fatalf("read the claims back: %v", err)
			}
			var restored *supervise.Delivery
			for _, c := range claims {
				if c.Token == r.reviewer.Token {
					restored = c.Pending
				}
			}
			if restored == nil || restored.NewsOnly != tc.newsOnly {
				t.Fatalf("the claim store restored %+v, want the task with NewsOnly %t", restored, tc.newsOnly)
			}
			phaseTask := !tc.newsOnly
			if worked := restored.StillWorked(task.Generation, task.Phase, supervise.StateReady); worked != phaseTask {
				t.Fatalf("the restored task is still worked as its phase's task %t, want %t", worked, phaseTask)
			}
			if holds := supervise.HoldsWork(restored); holds != phaseTask {
				t.Fatalf("the restored task holds work %t, want %t", holds, phaseTask)
			}
			var held map[claim.Token]*record.RoleClaim
			if err := pgx.BeginFunc(r.ctx, r.store.Pool(), func(tx pgx.Tx) error {
				var err error
				held, err = record.NewStore().RoleClaims(r.ctx, tx, []claim.Token{r.reviewer.Token})
				return err
			}); err != nil {
				t.Fatalf("read the role claim: %v", err)
			}
			run := held[r.reviewer.Token]
			if run == nil || run.Pending == nil || run.Pending.NewsOnly != tc.newsOnly {
				t.Fatalf("the role run read %+v, want its pending task with NewsOnly %t", run, tc.newsOnly)
			}
			if holds := supervise.HoldsWork(run.Pending); holds != phaseTask {
				t.Fatalf("the role run's pending task holds work %t, want %t", holds, phaseTask)
			}
		})
	}
}
