package daemon

import (
	"context"
	"encoding/json"
	"strings"
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
	holdSlot(t, pool, filler)
	// The admission pass reads the architect's claim row before it writes the notice
	// (record.Store.RoleRun, admit's tellWaitingForSlot), while the publish side reads the
	// supervisor's own copy in memory (outboxClaimStore), so the architect is recorded in both.
	seedSessionClaim(t, pool, string(mustClaimToken(t, root, claim.RoleArchitect)), "LEGION", root, root, "ses_architect")
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

// The window the test above opens reaches past one resume. While the first notice is still held
// for an architect that holds no role, the root is resumed, hands another question to a person,
// is answered again and is told again: the record then carries an answer and the mark once more,
// of the second wait. Read without the wait each row was written for, both rows would publish —
// the older one first (earlierNoticeFor), naming the old answer and a place from before the
// resume — and one wait would earn two notices. Each row names the moment its own wait was
// answered (record.WaitingForSlot), so the held row finishes without publishing and the
// architect is told once, of the wait its root stands in.
func TestAHeldWaitingForSlotNoticeIsNotPublishedForTheRootsNextWait(t *testing.T) {
	ctx := context.Background()
	pool := isolatedOutboxPool(t)
	records := record.NewStore()
	const root, filler = "LEGION-1", "LEGION-900"
	first := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	putOutboxIssue(t, pool, records, slotNoticeIssue(filler, "A", phase.Implementing, nil))
	putOutboxIssue(t, pool, records, slotNoticeIssue(root, "B", phase.AwaitingPerson, &record.Awaiting{
		Ask: "ask-q", AnsweredAt: &first, AnsweredBy: record.AnsweredByAsk, AnsweredDetail: "ask-q",
		Since: record.EventMark{Seq: 5},
	}))
	outboxTreeStore(t, pool, root, filler)
	holdSlot(t, pool, filler)
	seedSessionClaim(t, pool, string(mustClaimToken(t, root, claim.RoleArchitect)), "LEGION", root, root, "ses_architect")
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	architectClaim(t, sup, root, supervise.StateWorking)
	engine := workflow.New(records, workflow.Config{Project: "LEGION", Repositories: testRepositories, Steps: config.DesignGateRootIssues.Steps()}, quietLogger())
	handlers := []intake.Handler{engine, admit.New(records, engine, 1, "LEGION", quietLogger())}
	publisher := &holderPublisher{}
	publisher.setAbsent(architectTopic(t, root))
	clock := time.Now()
	runner := &outbox{log: quietLogger(), pool: pool, dispatchProject: "LEGION", records: records,
		notices: publisher, supervisor: sup, project: "legion", now: func() time.Time { return clock }}

	// The first wait is told, and the publish is refused because nobody holds the role: the row
	// is held for the architect it was written to.
	tick(t, pool, handlers, "the-pass-that-finds-no-slot")
	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("run the outbox while nobody holds the architect's role: %v", err)
	}
	if held := waitingForSlotNotices(t, pool, root); len(held) != 1 {
		t.Fatalf("waiting-for-slot rows after the refused publish = %#v, want the one row held for its architect", held)
	}
	if delivered := publishedWaitingForSlot(t, publisher); len(delivered) != 0 {
		t.Fatalf("waiting-for-slot publishes while nobody holds the role = %#v, want none: the row is held", delivered)
	}

	// A slot frees, the root is resumed, and the held row is still queued behind that.
	finish(t, pool, records, filler)
	tick(t, pool, handlers, "the-pass-that-resumes-the-root")
	runResumeStart(t, pool, root)

	// The root hands another question to a person and a person answers it: a second wait, with
	// its own answer and no mark. It gives its slot back as the transition into awaiting_person
	// does (record.Store.ReleaseSlot), and the root that was finished above is at work again and
	// holds the cap's one slot, so the pass that follows finds the second wait with nothing free.
	resumed := readIssue(t, pool, root)
	resumed.Phase = phase.AwaitingPerson
	resumed.Awaiting = &record.Awaiting{Ask: "ask-r", AnsweredAt: &second, AnsweredBy: record.AnsweredByAsk,
		AnsweredDetail: "ask-r", Since: record.EventMark{Seq: 9}}
	if err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := records.ReleaseSlot(ctx, tx, root); err != nil {
			return err
		}
		return records.PutIssue(ctx, tx, *resumed)
	}); err != nil {
		t.Fatalf("put %s into a second wait of its own: %v", root, err)
	}
	putOutboxIssue(t, pool, records, slotNoticeIssue(filler, "A", phase.Implementing, nil))
	holdSlot(t, pool, filler)

	tick(t, pool, handlers, "the-pass-that-finds-no-slot-for-the-second-wait")
	if held := waitingForSlotNotices(t, pool, root); len(held) != 2 {
		t.Fatalf("waiting-for-slot rows after the second wait was told = %#v, want two: the row held from the first wait and the second wait's own", held)
	}
	if got := readIssue(t, pool, root); got.Awaiting == nil || got.Awaiting.SlotNoticeAt == nil ||
		!got.Awaiting.AnsweredAt.Equal(second) {
		t.Fatalf("the wait of %s after the second pass = %#v, want the second wait carrying the mark that says it was told", root, got.Awaiting)
	}

	// The architect holds its role again. Both rows run: the second pass is there because the
	// fence holds the later row behind the earlier one until that one is finished
	// (earlierNoticeFor), whichever order the runner leases them in.
	publisher.setAbsent()
	clock = clock.Add(2 * time.Minute)
	for pass := 1; pass <= 2; pass++ {
		if err := runner.RunOnce(ctx); err != nil {
			t.Fatalf("run the outbox after the second wait was told (pass %d): %v", pass, err)
		}
	}
	delivered := publishedWaitingForSlot(t, publisher)
	if len(delivered) != 1 {
		t.Fatalf("waiting-for-slot publishes after the second wait was told = %#v, want exactly one: one wait earns one notice", delivered)
	}
	if !strings.Contains(delivered[0].Reason, "ask-r") {
		t.Fatalf("the notice published reads %q, want the one of the wait the root stands in (ask-r): the held row speaks of a wait that is over", delivered[0].Reason)
	}
	if left := waitingForSlotNotices(t, pool, root); len(left) != 0 {
		t.Fatalf("waiting-for-slot rows after both ran = %#v, want both finished", left)
	}
}

// slotNoticeIssue is a root of project LEGION, its own tree, at phase and rank, waiting on
// waiting (nil for a root that waits on nobody).
func slotNoticeIssue(key, rank string, at phase.Phase, waiting *record.Awaiting) record.Issue {
	return record.Issue{Key: key, Repo: "acme/widgets", DefaultBranch: "main", GitHubRepo: "acme/widgets",
		Project: "LEGION", Tree: key, Title: key, Phase: at, Generation: 1, Status: "in_progress",
		Rank: rank, HandedOver: true, LastDispatchSeq: 1, Awaiting: waiting}
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
