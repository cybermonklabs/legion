package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sjawhar/legion/daemon/internal/admit"
	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/config"
	"github.com/sjawhar/legion/daemon/internal/intake"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/supervise"
	"github.com/sjawhar/legion/daemon/internal/workflow"
)

// The notice and the resume it speaks of travel on lanes nothing orders against each other, so a
// held notice can outlive the wait it describes: the admission pass writes it while the
// architect's claim holds its Envoy role, the session drops before the publish lands, the
// listener refuses the publish because no session holds the role, a slot frees, and the next
// pass resumes that architect with the task of the work it waited for. The held row must not
// then tell a working architect to do nothing until the daemon starts it: its root has left the
// wait (stillWaitsForSlot), so the row finishes without publishing.
func TestAHeldWaitingForSlotNoticeDoesNotReachAResumedArchitect(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, filler = "LEGION-1", "LEGION-900"
	answered := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	// The one slot of the cap is taken by a root at work, and the root this test follows stands
	// in awaiting_person with a person's answer on its wait and nothing in the column the mark
	// lives in: the wait no pass has told anyone about yet.
	putOutboxIssue(t, pool, records, slotNoticeIssue(filler, "A", phase.Implementing, nil))
	putOutboxIssue(t, pool, records, slotNoticeIssue(root, "B", phase.AwaitingPerson, &record.Awaiting{
		Ask: "ask-q", AnsweredAt: &answered, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q",
		Since: record.EventMark{Seq: 5},
	}))
	outboxTreeStore(t, pool, root, filler)
	putSlotRow(t, pool, records, filler)
	architectClaimRow(t, pool, root)
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, root, supervise.StateWorking)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	handlers := []intake.Handler{engine, admit.New(records, engine, 1, "LEGION", quietLogger())}
	publisher := &holderPublisher{}
	// Nobody holds the architect's role at the publish: its session dropped between the pass that
	// wrote the notice and the attempt that would deliver it.
	publisher.setAbsent(architectTopic(t, root))
	// The runner reads its own clock, which the test moves past the backoff the refused publish
	// writes into the row, so the attempt after the resume runs on the row rather than finding
	// it not yet due.
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: func() time.Time { return clock }}

	tick(t, pool, handlers, "the-pass-that-finds-no-slot")
	if told := waitingForSlotNotices(t, pool, root); len(told) != 1 {
		t.Fatalf("waiting-for-slot notices after the pass that found no slot = %#v, want the one this wait earns", told)
	}
	if got := readIssue(t, pool, root); got.Awaiting == nil || got.Awaiting.SlotNoticeAt == nil {
		t.Fatalf("the wait of %s after the pass = %#v, want the mark that says its architect was told", root, got.Awaiting)
	}
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox while nobody holds the architect's role: %v", err)
	}
	if delivered := publishedWaitingForSlot(t, publisher); len(delivered) != 0 {
		t.Fatalf("waiting-for-slot publishes while nobody holds the role = %#v, want none: the row is held", delivered)
	}
	if held := waitingForSlotNotices(t, pool, root); len(held) != 1 {
		t.Fatalf("waiting-for-slot rows after the refused publish = %#v, want the one row held for its architect", held)
	}

	// The slot frees and the next pass resumes the root: its architect is relaunched with the
	// task of the work the person answered, and holds its role again.
	finish(t, pool, records, filler)
	tick(t, pool, handlers, "the-pass-that-resumes-the-root")
	if got := readIssue(t, pool, root); got.Phase != phase.Admitted || got.Awaiting != nil {
		t.Fatalf("the root after the pass that resumes it = phase %s with wait %#v, want it admitted with no wait", got.Phase, got.Awaiting)
	}
	runResumeStart(t, pool, root)
	publisher.setAbsent()
	clock = clock.Add(2 * time.Minute)

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox after the resume: %v", err)
	}
	if delivered := publishedWaitingForSlot(t, publisher); len(delivered) != 0 {
		t.Fatalf("waiting-for-slot publishes after the resume = %#v, want none: the root the notice speaks of is working again", delivered)
	}
	if left := waitingForSlotNotices(t, pool, root); len(left) != 0 {
		t.Fatalf("waiting-for-slot rows after the resume = %#v, want the held row finished rather than kept for a later attempt", left)
	}
}

// slotNoticeIssue is a root of project LEGION, its own tree, at phase and rank, waiting on
// waiting (nil for a root that waits on nobody).
func slotNoticeIssue(key, rank string, at phase.Phase, waiting *record.Awaiting) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		Rank: rank, HandedOver: true, LastDispatchSeq: 1, Awaiting: waiting}
}

// putSlotRow gives issue the admission slot its promotion granted it, as the fixtures of a root
// already at work do (holdSlot).
func putSlotRow(t *testing.T, pool *pgxpool.Pool, records record.Store, issue string) {
	t.Helper()
	ctx := context.Background()
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		slots, err := records.Slots(ctx, tx)
		if err != nil {
			return err
		}
		return records.PutSlot(ctx, tx, record.Slot{Issue: issue, Index: len(slots), AdmittedAt: time.Now().UTC()})
	}); err != nil {
		t.Fatalf("give %s its admission slot: %v", issue, err)
	}
}

// architectClaimRow records issue's root architect working in the claims table, at the issue's
// generation: the row an admission pass reads before it writes a waiting-for-slot notice
// (record.Store.RoleRun, admit's tellWaitingForSlot). The supervisor of this package's fixtures
// keeps its claims in memory (outboxClaimStore), which is what the publish side reads, so the
// architect is recorded in both.
func architectClaimRow(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `insert into claims (token, project, tree, issue, role, generation, session,
		session_file, state, launch_failures, prompt_failures, prompt_retires, uncertain_streak)
		values ($1, 'LEGION', $2, $2, 'architect', 1, 'ses_architect', '', 'working', 0, 0, 0, 0)`,
		string(mustClaimToken(t, issue, claim.RoleArchitect)), issue); err != nil {
		t.Fatalf("record the architect claim of %s: %v", issue, err)
	}
}

// runResumeStart stands in for the outbox running the resume's architect start: the row goes, as
// the lifecycle lane's own runner would take it. This fixture's runtime launches nothing, and
// the start is not what this test follows.
func runResumeStart(t *testing.T, pool *pgxpool.Pool, issue string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `delete from outbox where issue = $1 and kind = 'supervise'`, issue); err != nil {
		t.Fatalf("run the architect start of %s: %v", issue, err)
	}
}

// waitingForSlotNotices is every waiting-for-slot notice the outbox still holds for issue, oldest
// first.
func waitingForSlotNotices(t *testing.T, pool *pgxpool.Pool, issue string) []record.Notice {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `select payload::text from outbox where issue = $1 and kind = 'notice'
		and payload->>'kind' = $2 order by id`, issue, string(workflow.WaitingForSlotNoticeKind))
	if err != nil {
		t.Fatalf("read the waiting-for-slot rows of %s: %v", issue, err)
	}
	defer rows.Close()
	var held []record.Notice
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			t.Fatalf("read a waiting-for-slot row of %s: %v", issue, err)
		}
		var notice record.Notice
		if err := json.Unmarshal([]byte(payload), &notice); err != nil {
			t.Fatalf("decode a waiting-for-slot row of %s: %v", issue, err)
		}
		held = append(held, notice)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the waiting-for-slot rows of %s: %v", issue, err)
	}
	return held
}

// publishedWaitingForSlot is every waiting-for-slot notice the publisher delivered, in order.
func publishedWaitingForSlot(t *testing.T, publisher *holderPublisher) []record.Notice {
	t.Helper()
	_, delivered := publisher.snapshot()
	var got []record.Notice
	for _, published := range delivered {
		if notice, ok := published.payload.(record.Notice); ok && notice.Kind == workflow.WaitingForSlotNoticeKind {
			got = append(got, notice)
		}
	}
	return got
}
