package admit

import (
	"context"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/intake"
)

// pauseTestProject opens a pause of the package's project as the operator's pause writes its row
// (store's Pause, project_pauses), and answers its id.
func pauseTestProject(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `insert into project_pauses (project, began_at, by_kind, reason)
		values ($1, $2, 'operator', 'the model provider is down') returning id`, testProject, fixedNow).Scan(&id); err != nil {
		t.Fatalf("pause %s: %v", testProject, err)
	}
	return id
}

// endTestPause ends pause id as the operator's resume ends it (store's Resume).
func endTestPause(t *testing.T, pool *pgxpool.Pool, id int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `update project_pauses
		set ended_at = $2, ended_by_kind = 'operator', ended_by_session = '' where id = $1`, id, fixedNow); err != nil {
		t.Fatalf("resume %s: %v", testProject, err)
	}
}

// slottedKeys is the issues holding one of the cap's slots, in slot order.
func slottedKeys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	for _, slot := range slots(t, pool) {
		keys = append(keys, slot.Issue)
	}
	return keys
}

// A paused project's admission passes promote nothing and send no role the work of its entry
// (decision 11 of dispatch://INFRA-3759): the tick's pass, the entry sweep and a live fact's pass
// each return at their top while an open pause names the project, so a root waiting in todo with
// a slot free keeps waiting, and a worker behind the entry its phases row names is sent nothing.
// The tick still wakes the controller. Once the pause ends, the same tick promotes the root and
// sends the entry's work: that is what tells the pause's gate from a fixture that would have
// promoted nothing anyway.
func TestAPausedProjectPromotesNothingUntilItsPauseEnds(t *testing.T) {
	pool := migratedPool(t)
	behind, taken := seedConvergeRows(t, pool, convergeRows()[:1])
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	registerController(t, pool)
	admission := quiet(t, taken+1)
	held := slottedKeys(t, pool)

	pause := pauseTestProject(t, pool)
	tick(t, pool, admission, "paused-tick")
	applySweep(t, pool, admission, "paused-sweep")
	apply(t, pool, admission, "paused-fact", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: "LEGION-NEXT", Rank: "B", HandedOver: handed}, engineStub{})

	if got := slottedKeys(t, pool); !slices.Equal(got, held) {
		t.Fatalf("slots while paused = %v, want %v alone: a paused project promotes nothing", got, held)
	}
	if got := issue(t, pool, "LEGION-NEXT"); got.Status != "todo" {
		t.Fatalf("LEGION-NEXT stands %q while paused, want todo: it was promoted", got.Status)
	}
	for _, c := range behind {
		if starts := convergeStarts(t, pool, c.key); len(starts) != 0 {
			t.Fatalf("start rows of %s while paused = %+v, want none: a paused project sends no owed entry", c.key, starts)
		}
	}
	tickNoticeOf(t, pool, testProject)

	endTestPause(t, pool, pause)
	tick(t, pool, admission, "resumed-tick")
	if got := slottedKeys(t, pool); !slices.Equal(got, append(held, "LEGION-NEXT")) {
		t.Fatalf("slots once the pause ended = %v, want %v and LEGION-NEXT", got, held)
	}
	assertConverged(t, pool, behind)
}

// The resume's one admission pass is a fact of its own (intake.ProjectResumed, decision 22 of
// dispatch://INFRA-3759): admission promotes on it as on every fact that is not a Dispatch issue,
// so the root the pause kept waiting takes the free slot at once. It is no controller tick: the
// controller is registered, and no tick notice is queued.
func TestTheResumesPassPromotesAndQueuesNoTickNotice(t *testing.T) {
	pool := migratedPool(t)
	seedWaiting(t, pool, "LEGION-NEXT", "B")
	registerController(t, pool)
	admission := quiet(t, 1)
	pause := pauseTestProject(t, pool)
	apply(t, pool, admission, "paused-fact", intake.DispatchIssue{Key: "LEGION-NEXT", ResolvedRepo: "acme/widgets", Seq: 2,
		Type: "issue.updated", Status: "todo", Title: "LEGION-NEXT", Rank: "B", HandedOver: handed}, engineStub{})
	if got := slottedKeys(t, pool); len(got) != 0 {
		t.Fatalf("slots while paused = %v, want none", got)
	}

	endTestPause(t, pool, pause)
	if _, err := intake.ApplyFact(context.Background(), pool, "pause", "project-resumed", intake.ProjectResumed{Pause: pause},
		engineStub{}, admission); err != nil {
		t.Fatalf("apply the resume's pass: %v", err)
	}

	if got := slottedKeys(t, pool); !slices.Equal(got, []string{"LEGION-NEXT"}) {
		t.Fatalf("slots after the resume's pass = %v, want LEGION-NEXT: the pass promotes", got)
	}
	var ticks int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox
		where kind = 'controller_notice' and payload->>'kind' = 'tick'`).Scan(&ticks); err != nil {
		t.Fatalf("read the tick notices: %v", err)
	}
	if ticks != 0 {
		t.Fatalf("tick notices after the resume's pass = %d, want none: the resume wakes nobody by itself", ticks)
	}
}
