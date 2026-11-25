package admit

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// News of the approved design alone is no work a root architect holds (dispatch://INFRA-175
// meeting dispatch://INFRA-2614): a start queued for it (record.SuperviseRequest.News) or a task
// of it the claim has not taken (supervise.Delivery.NewsOnly) carries no entry, raises none and
// hands the architect none of the work its phases row names. So the sweep sends a root's architect
// the work of the entry its row names while the architect holds only news, as it does for an
// architect holding nothing (workflow.ArchitectStartPending), and the entry's task then takes the
// untaken news task's place in the claim's hands, carrying that news itself (supervise's
// Machine.supersedes). Held back by the news, the sweep would write nothing for as long as the
// news waited behind a turn of the architect's own, and the architect's owed entry would wait
// behind the news.
//
// Each case is the sweep's base fixture (architectSweepRoot): a root at admitted holding its slot,
// its architect's row at seededEntry, and an idle architect claim given the entry before it. The
// control holds a queued start that brings the architect work, a wake's, which holds the sweep's
// start back, so the case that sends is sending for the reason it names.
func TestTheArchitectSweepSendsTheOwedEntryWhileTheArchitectHoldsOnlyNews(t *testing.T) {
	const root = "LEGION-840"
	queue := func(t *testing.T, pool *pgxpool.Pool, request record.SuperviseRequest) {
		t.Helper()
		inTx(t, pool, func(tx pgx.Tx) {
			row, err := record.NewOutboxRow(root, request, fixedNow)
			if err != nil {
				t.Fatalf("build the start of %s's architect: %v", root, err)
			}
			if err := record.NewStore().Enqueue(context.Background(), tx, row); err != nil {
				t.Fatalf("queue the start of %s's architect: %v", root, err)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		hold  func(t *testing.T, pool *pgxpool.Pool, token claim.Token)
		sends bool
		why   string
	}{
		{
			name: "a task of news alone the architect has not taken",
			hold: func(t *testing.T, pool *pgxpool.Pool, token claim.Token) {
				t.Helper()
				if _, err := pool.Exec(context.Background(), `insert into pending_task_deliveries (claim_token, delivery_id, task, queued_at,
					generation, phase, design_news, news_only) values ($1, 'outbox:12', 'The approved design of LEGION-840 changed.', $2, $3, '',
					'{"task":""}'::jsonb, true)`, string(token), fixedNow, int64(architectSweepGeneration)); err != nil {
					t.Fatalf("record the news task %s holds: %v", token, err)
				}
			},
			sends: true,
			why:   "the task holds no entry's work, so no start of the row's entry is on its way",
		},
		{
			name: "a start of news alone queued",
			hold: func(t *testing.T, pool *pgxpool.Pool, _ claim.Token) {
				t.Helper()
				queue(t, pool, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
					Generation: architectSweepGeneration, News: true})
			},
			sends: true,
			why:   "a start of news alone carries no entry, so it is no start of the row's entry on its way",
		},
		{
			name: "control: a wake's start queued",
			hold: func(t *testing.T, pool *pgxpool.Pool, _ claim.Token) {
				t.Helper()
				queue(t, pool, record.SuperviseRequest{Op: "start", Tree: root, Role: claim.RoleArchitect,
					Generation: architectSweepGeneration, Task: "Your pod was released and has come back."})
			},
			why: "a start that brings the architect work is on its way, and the claim sends it first",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := migratedPool(t)
			admission := quiet(t, 1)
			seedArchitectSweepRoot(t, pool, architectSweepRoot(root))
			project, err := claim.ProjectToken(testProject)
			if err != nil {
				t.Fatal(err)
			}
			token, err := claim.NewToken(project, root, claim.RoleArchitect)
			if err != nil {
				t.Fatal(err)
			}
			tc.hold(t, pool, token)

			applySweep(t, pool, admission, "architect-news")

			var sent []convergeStart
			for _, start := range architectSweepStarts(t, pool, root) {
				if start.entry == seededEntry {
					sent = append(sent, start)
				}
			}
			if tc.sends && (len(sent) != 1 || sent[0].task == "") {
				t.Fatalf("the starts of entry %d the sweep wrote for %s = %+v, want one carrying its work: %s", seededEntry, root, sent, tc.why)
			}
			if !tc.sends && len(sent) != 0 {
				t.Fatalf("the sweep wrote %+v for %s, want nothing: %s", sent, root, tc.why)
			}
		})
	}
}
