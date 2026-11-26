package daemon

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// loopHook is a second hook firing on the move back firedHook fired on, as two hooks of the
// deployment firing on one move back queue two prompts for one role.
var loopHook = record.HookPrompt{Hook: "review-loop", Role: claim.RoleImplementer,
	From: phase.Reviewing, To: phase.Implementing, Count: 3,
	Counts: map[string]int{"reviewing-implementing": 3, "testing-implementing": 1},
	Prompt: "Say what the reviewer asked for last time and what you did about it."}

// unheldHookRunner is hookRunner against a listener that keeps nothing for a role no live session
// holds, as the listener answers a publish that names no source session: every publish to the
// implementer's topic is refused notify.ErrNoHolder until the test hands the role to a session
// (holderPublisher.setAbsent with no topic).
func unheldHookRunner(t *testing.T) (*outbox, *supervisor, *store.Pool, *holderPublisher, string) {
	t.Helper()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
	publisher.setAbsent(topic)
	return runner, sup, pool, publisher, topic
}

// hookRowMadeAt enqueues payload's hook-prompt row for LEGION-208 as made at made, due now.
func hookRowMadeAt(t *testing.T, runner *outbox, pool *store.Pool, payload record.HookPrompt, made time.Time) {
	t.Helper()
	row := mustOutboxRow(t, "LEGION-208", payload, time.Now())
	row.CreatedAt = made
	enqueueOutbox(t, pool, runner.records, row)
}

// rowsOf is how many rows of kind the outbox holds for LEGION-208.
func rowsOf(t *testing.T, pool *store.Pool, kind record.OutboxKind) int {
	t.Helper()
	var rows int
	if err := pool.QueryRow(context.Background(), `select count(*) from outbox where issue = 'LEGION-208' and kind = $1`, string(kind)).Scan(&rows); err != nil {
		t.Fatalf("count the %s rows: %v", kind, err)
	}
	return rows
}

// issueMessages is the body of every Dispatch message the outbox holds for LEGION-208, in order.
func issueMessages(t *testing.T, pool *store.Pool) []string {
	t.Helper()
	var bodies []string
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select payload->>'body' from outbox where issue = 'LEGION-208' and kind = $1 order by id`,
			string(record.OutboxKindDispatchMessage))
		if err != nil {
			return err
		}
		bodies, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		t.Fatalf("read the Dispatch messages of LEGION-208: %v", err)
	}
	return bodies
}

// issueMessageRow is one Dispatch message row the outbox holds for LEGION-208: its body and when it
// is due to post.
type issueMessageRow struct {
	body string
	due  time.Time
}

// heldMessages is every Dispatch message row the outbox holds for LEGION-208, in order.
func heldMessages(t *testing.T, pool *store.Pool) []issueMessageRow {
	t.Helper()
	var held []issueMessageRow
	if err := store.Transact(context.Background(), pool, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `select payload->>'body', next_at from outbox where issue = 'LEGION-208' and kind = $1 order by id`,
			string(record.OutboxKindDispatchMessage))
		if err != nil {
			return err
		}
		held, err = pgx.CollectRows(rows, func(row pgx.CollectableRow) (issueMessageRow, error) {
			var message issueMessageRow
			err := row.Scan(&message.body, &message.due)
			return message, err
		})
		return err
	}); err != nil {
		t.Fatalf("read the Dispatch messages of LEGION-208: %v", err)
	}
	return held
}

// requireDueAt fails unless message is due at want, to the database's microsecond.
func requireDueAt(t *testing.T, message issueMessageRow, want time.Time, what string) {
	t.Helper()
	if gap := message.due.Sub(want); gap > time.Millisecond || gap < -time.Millisecond {
		t.Fatalf("%s is due at %s, want %s: the message is held for the hour from the first of its prompts to end", what, message.due.UTC(), want.UTC())
	}
}

// restartedHookRunner is a runner over pool as a daemon that restarted reads it: a new outbox and a
// new supervisor with no claim, publishing to publisher and posting to posted, at the wall clock.
// Nothing of an earlier runner carries over but what the database holds.
func restartedHookRunner(t *testing.T, pool *store.Pool, publisher notify.Publisher, posted *outboxDispatch) (*outbox, *supervisor) {
	t.Helper()
	sup, _ := newOutboxSupervisor(t, "legion", t.TempDir())
	runner := &outbox{
		dispatchProject: "LEGION", pool: pool, records: record.NewStore(), supervisor: sup, trees: outboxTreeStore(t, pool),
		tokens: outboxTokens{}, project: "legion", stateDir: t.TempDir(), notices: publisher, dispatch: posted,
		now: time.Now, log: quietLogger(),
	}
	return runner, sup
}

// failedClaim creates role's claim on LEGION-208 in sup as failed, so no session of it will hold
// the role again.
func failedClaim(t *testing.T, sup *supervisor, role claim.Role) {
	t.Helper()
	if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", role),
		Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: role, Generation: 1, ServingGeneration: 1,
		State: supervise.StateFailed}, ""); err != nil {
		t.Fatalf("create the failed %s's claim: %v", role, err)
	}
}

// requireSays fails unless body holds every one of words.
func requireSays(t *testing.T, body string, words ...string) {
	t.Helper()
	for _, word := range words {
		if !strings.Contains(body, word) {
			t.Fatalf("the message on the issue is\n\t%s\nwant it to say %q", body, word)
		}
	}
}

// A hook prompt the listener refuses because no live session holds its role (a listener that keeps
// nothing for a publish naming no source session, as the daemon's prompts name none) waits for a
// session to hold the role while the hour after its row was made runs: retried on the row's
// backoff, nothing posted on the issue. The listener keeps nothing for such a publish, so the row
// is the prompt's keeper.
func TestAHookPromptNoSessionHoldsWaitsForTheRoleInsideTheHour(t *testing.T) {
	t.Parallel()
	runner, _, pool, publisher, _ := unheldHookRunner(t)
	hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Minute))

	if err := runner.RunOnce(context.Background()); err != nil {
		t.Fatalf("drain the hook prompt: %v", err)
	}

	if attempts, delivered := publisher.snapshot(); attempts != 1 || len(delivered) != 0 || len(publisher.keptCopies()) != 0 {
		t.Fatalf("publish attempts = %d, %d delivered, %d kept; want the one refused attempt", attempts, len(delivered), len(publisher.keptCopies()))
	}
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 {
		t.Fatalf("hook-prompt rows left = %d, want the row waiting for a session to hold the role", left)
	}
	var waited string
	if err := pool.QueryRow(context.Background(), `select last_error from outbox where kind = $1`, string(record.OutboxKindHookPrompt)).Scan(&waited); err != nil {
		t.Fatalf("read the row's last error: %v", err)
	}
	if !strings.Contains(waited, "waits for a session to hold") {
		t.Fatalf("the row's last error is %q, want the wait for a session to hold the role", waited)
	}
	if posted := issueMessages(t, pool); len(posted) != 0 {
		t.Fatalf("messages on the issue = %q, want none inside the hour", posted)
	}
}

// Once the hour after its row was made has passed, the prompt's row finishes before anything
// publishes it, and a Dispatch message on the issue names it, in the form of the hook's own firing
// message: the hook, the role and the issue, why the prompt ended, the move back it was written
// for and its words, and that no session got them and nothing sends them again. The message is
// held for an hour from the prompt's end, so later prompts of the role join it. The control is the
// same row at the base, which finishes at its first refusal with no message.
func TestAHookPromptNoSessionHeldForAnHourEndsWithAMessageOnTheIssue(t *testing.T) {
	t.Parallel()
	runner, _, pool, publisher, _ := unheldHookRunner(t)
	hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Hour-time.Minute))

	ended := drainAt(t, runner, 0, "drain the hook prompt")

	if attempts, delivered := publisher.snapshot(); attempts != 0 || len(delivered) != 0 {
		t.Fatalf("publish attempts = %d with %d delivered, want none for a prompt past its hour", attempts, len(delivered))
	}
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
		t.Fatalf("hook-prompt rows left = %d, want the row finished", left)
	}
	held := heldMessages(t, pool)
	if len(held) != 1 {
		t.Fatalf("messages on the issue = %+v, want one", held)
	}
	t.Logf("the message on the issue: %s", held[0].body)
	requireSays(t, held[0].body, "Legion hook prompts to the implementer of LEGION-208 were not delivered.",
		"Legion hook back-to-implementing fired at 3 moves back: reviewing to implementing.",
		"the hour after the hook fired passed and no session of the implementer took it", firedHook.Prompt,
		"No session of the implementer received", "will not send")
	requireDueAt(t, held[0], ended.Add(time.Hour), "the message")
}

// A role whose claim failed or was retired has no session coming back for it, so its prompt ends
// at its first refusal, inside the hour, with the message on the issue naming that end. The control
// is the base, which finishes the row with no message.
func TestAHookPromptToARoleThatIsNotComingEndsAtOnceWithAMessageOnTheIssue(t *testing.T) {
	t.Parallel()
	for _, state := range []supervise.ClaimState{supervise.StateFailed, supervise.StateRetired} {
		t.Run(string(state), func(t *testing.T) {
			runner, sup, pool, _, _ := unheldHookRunner(t)
			if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleImplementer),
				Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleImplementer, Generation: 1, ServingGeneration: 1,
				State: state}, ""); err != nil {
				t.Fatalf("create the %s implementer's claim: %v", state, err)
			}
			hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Minute))

			if err := runner.RunOnce(context.Background()); err != nil {
				t.Fatalf("drain the hook prompt: %v", err)
			}

			if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
				t.Fatalf("hook-prompt rows left = %d, want the row finished at once", left)
			}
			posted := issueMessages(t, pool)
			if len(posted) != 1 {
				t.Fatalf("messages on the issue = %q, want one", posted)
			}
			requireSays(t, issueMessages(t, pool)[0], "Legion hook back-to-implementing", "LEGION-208",
				"the implementer's claim is "+string(state)+", so no session of it will hold the role again", firedHook.Prompt)
		})
	}
}

// One message on an issue names every prompt to one role that ends undelivered in the hour from
// the first of them to end, however far apart they end and across a restart of the daemon: the
// first end writes the message, held for that hour, and each later end adds to it. A prompt to
// another role gets a message of its own, and a prompt to the same role that ends after the hour
// opens a new one. The control is the base, which finishes the first row at its refusal and posts
// nothing; and the head before the window, which posts a message for each end at once, so two
// prompts ending 20 minutes apart post two.
func TestTheHookPromptsOfOneRoleThatEndWithinAnHourShareOneMessage(t *testing.T) {
	t.Parallel()
	publisher := &holderPublisher{}
	runner, _, pool := hookRunner(t, publisher)
	posted := &outboxDispatch{}
	runner.dispatch = posted
	publisher.setAbsent(notify.RoleTopicPrefix+string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer)),
		notify.RoleTopicPrefix+string(mustClaimToken(t, "LEGION-208", claim.RoleTester)))
	testerHook := firedHook
	testerHook.Role, testerHook.Hook, testerHook.Prompt = claim.RoleTester, "tester-loop", "Run the failing case first."
	againHook := firedHook
	againHook.Hook, againHook.Prompt = "back-again", "Start from the failing test."

	// 1. The first prompt to the implementer ends at the end of its own hour, and the message
	//    that names it is held for an hour from then.
	hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Hour-time.Minute))
	opened := drainAt(t, runner, 0, "the first prompt's end")
	held := heldMessages(t, pool)
	if len(held) != 1 {
		t.Fatalf("after the first prompt's end the outbox holds messages %+v, want one", held)
	}
	requireDueAt(t, held[0], opened.Add(time.Hour), "the implementer's message")

	// 2. The daemon restarts, and the implementer's and the tester's claims fail. Two prompts
	//    fired after the first one ended, one to each role, end 20 minutes after it.
	restarted, sup := restartedHookRunner(t, pool, publisher, posted)
	failedClaim(t, sup, claim.RoleImplementer)
	failedClaim(t, sup, claim.RoleTester)
	hookRowMadeAt(t, restarted, pool, loopHook, time.Now())
	hookRowMadeAt(t, restarted, pool, testerHook, time.Now())
	later := drainAt(t, restarted, 20*time.Minute, "the prompts that end 20 minutes later")
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
		t.Fatalf("hook-prompt rows left = %d, want both later prompts finished", left)
	}
	if len(posted.messages) != 0 {
		t.Fatalf("Dispatch was posted %q inside the implementer's hour, want nothing yet", posted.messages)
	}
	held = heldMessages(t, pool)
	if len(held) != 2 {
		t.Fatalf("the outbox holds messages %+v, want the implementer's and the tester's", held)
	}
	requireSays(t, held[0].body, "Legion hook prompts to the implementer of LEGION-208 were not delivered.",
		firedHook.Prompt, "the hour after the hook fired passed and no session of the implementer took it",
		loopHook.Prompt, "the implementer's claim is failed, so no session of it will hold the role again")
	requireDueAt(t, held[0], opened.Add(time.Hour), "the implementer's message, with its second prompt")
	requireSays(t, held[1].body, "Legion hook prompts to the tester of LEGION-208 were not delivered.", testerHook.Prompt)
	if strings.Contains(held[1].body, firedHook.Prompt) || strings.Contains(held[0].body, testerHook.Prompt) {
		t.Fatalf("a role's message names another role's prompt: %+v", held)
	}
	requireDueAt(t, held[1], later.Add(time.Hour), "the tester's message")

	// 3. The implementer's hour ends: its one message posts, naming both of its prompts, and the
	//    tester's waits for its own hour.
	drainAt(t, restarted, 61*time.Minute, "the end of the implementer's hour")
	if len(posted.messages) != 1 {
		t.Fatalf("Dispatch was posted %d messages at the end of the implementer's hour, want one: %q", len(posted.messages), posted.messages)
	}
	t.Logf("the message posted on the issue: %s", posted.messages[0])
	requireSays(t, posted.messages[0], "Legion hook prompts to the implementer of LEGION-208 were not delivered.",
		"Legion hook back-to-implementing fired", firedHook.Prompt, "Legion hook review-loop fired", loopHook.Prompt)
	if held = heldMessages(t, pool); len(held) != 1 || !strings.Contains(held[0].body, testerHook.Prompt) {
		t.Fatalf("after the implementer's message posted the outbox holds %+v, want the tester's alone", held)
	}

	// 4. A prompt to the implementer that ends after its hour opens a message of its own.
	hookRowMadeAt(t, restarted, pool, againHook, time.Now())
	third := drainAt(t, restarted, 70*time.Minute, "a prompt that ends after the implementer's hour")
	if len(posted.messages) != 1 {
		t.Fatalf("Dispatch was posted %q, want the implementer's first message alone", posted.messages)
	}
	held = heldMessages(t, pool)
	if len(held) != 2 || !strings.Contains(held[1].body, againHook.Prompt) || strings.Contains(held[1].body, firedHook.Prompt) {
		t.Fatalf("the outbox holds %+v, want the tester's message and a new one for the implementer naming its third prompt alone", held)
	}
	requireDueAt(t, held[1], third.Add(time.Hour), "the implementer's second message")
}

// A released role's woken session can claim its role only after several of the row's attempts:
// the first wakes the pod and waits, and each later one before the claim meets a role no live
// session holds, which the listener refuses with nothing kept. The row waits through each of them
// on its backoff, and its first attempt once the session holds the role delivers the prompt, once:
// a refused publish stores nothing, so the one forward is the one the session gets. The pod is
// woken once and nothing is posted on the issue. The control is the base, whose row finishes at
// the first refusal, the prompt kept nowhere.
func TestAHookPromptReachesItsWokenRoleAfterAClaimSlowerThanTwoRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runner, sup, pool, publisher, topic := unheldHookRunner(t)
	machine := releasedClaim(t, sup, claim.RoleImplementer)
	hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Minute))

	if err := runner.RunOnce(ctx); err != nil {
		t.Fatalf("the row's first attempt: %v", err)
	}
	if attempts, _ := publisher.snapshot(); attempts != 0 {
		t.Fatalf("the first attempt published %d times, want none: it wakes the released pod and waits", attempts)
	}
	if held := machine.Claim(); held.Released() {
		t.Fatalf("the implementer is %s by %s after the first attempt, want the wake to have resumed it", held.State, held.SuspendedBy)
	}
	// Two attempts, each past the row's backoff, while the woken session has claimed nothing.
	for step := 1; step <= 2; step++ {
		drainAt(t, runner, time.Duration(step)*time.Minute, "a retry before the claim")
		if attempts, delivered := publisher.snapshot(); attempts != step || len(delivered) != 0 {
			t.Fatalf("after retry %d: %d publishes, %d delivered; want %d refused publishes", step, attempts, len(delivered), step)
		}
		if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 {
			t.Fatalf("after retry %d: hook-prompt rows left = %d, want the row still waiting", step, left)
		}
	}
	// The woken session holds the role now.
	publisher.setAbsent()
	drainAt(t, runner, 3*time.Minute, "the attempt after the claim")

	attempts, delivered := publisher.snapshot()
	if attempts != 3 || len(delivered) != 1 || delivered[0].topic != topic || delivered[0].message != firedHook.Message("LEGION-208") {
		t.Fatalf("%d publishes delivered %+v, want the prompt delivered once to %s on the third", attempts, delivered, topic)
	}
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
		t.Fatalf("hook-prompt rows left = %d, want the row finished", left)
	}
	if posted := issueMessages(t, pool); len(posted) != 0 {
		t.Fatalf("messages on the issue = %q, want none: the prompt was delivered", posted)
	}
	if resumes := podResumes(t, pool, "LEGION-208"); resumes != 1 {
		t.Fatalf("recorded resumes = %d, want the one wake the row's first attempt took", resumes)
	}
}

// undeliveredNotices is the reason of every hook-prompt-undelivered notice for LEGION-208's
// architect, in order: a notice the drain that wrote it has already published is in the
// listener's record (publisher), and one it has not published is still an outbox row.
func undeliveredNotices(t *testing.T, pool *store.Pool, publisher *holderPublisher) []string {
	t.Helper()
	reasons := noticeReasons(t, pool, string(record.HookPromptUndeliveredNotice))
	_, delivered := publisher.snapshot()
	for _, published := range delivered {
		if notice, ok := published.payload.(record.Notice); ok && notice.Kind == record.HookPromptUndeliveredNotice {
			reasons = append(reasons, notice.Reason)
		}
	}
	return reasons
}

// A hook prompt that ends undelivered tells the issue's architect, once for each message on the
// issue that names such prompts: the prompt that opens a role's message writes the notice in the
// same transaction. A second prompt of that role that ends inside the message's hour joins the
// message and writes no notice, a prompt to another role opens a message and a notice of its own,
// and a prompt of the first role that ends after the hour opens a new message and a new notice. So
// a role that is not coming makes one notice an hour, as it makes one message. The control is the
// head before the notice, which wrote none: the first check fails there with two notices wanted
// and none found.
func TestAnUndeliveredHookPromptTellsTheIssuesArchitectOnceForEachMessage(t *testing.T) {
	t.Parallel()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	failedClaim(t, sup, claim.RoleImplementer)
	failedClaim(t, sup, claim.RoleTester)
	testerHook := firedHook
	testerHook.Role, testerHook.Hook, testerHook.Prompt = claim.RoleTester, "tester-loop", "Run the failing case first."
	againHook := firedHook
	againHook.Hook, againHook.Prompt = "back-again", "Start from the failing test."

	hookRowMadeAt(t, runner, pool, firedHook, time.Now())
	hookRowMadeAt(t, runner, pool, loopHook, time.Now())
	hookRowMadeAt(t, runner, pool, testerHook, time.Now())
	drainAt(t, runner, 0, "the ends of three prompts, two to the implementer and one to the tester")
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
		t.Fatalf("UNKNOWN: hook-prompt rows left = %d, want all three ended, so the notices test nothing", left)
	}
	told := undeliveredNotices(t, pool, publisher)
	if len(told) != 2 {
		t.Fatalf("the architect's hook-prompt-undelivered notices = %q, want two: one for the implementer's message and one for the tester's", told)
	}
	byRole := map[claim.Role]string{}
	for _, reason := range told {
		for _, role := range []claim.Role{claim.RoleImplementer, claim.RoleTester} {
			if strings.Contains(reason, "to the "+string(role)+" of LEGION-208") {
				byRole[role] = reason
			}
		}
	}
	requireSays(t, byRole[claim.RoleImplementer], firedHook.Hook, "the implementer's claim is failed", "posts at")
	requireSays(t, byRole[claim.RoleTester], testerHook.Hook, "the tester's claim is failed")
	if strings.Contains(strings.Join(told, "\n"), loopHook.Hook) {
		t.Fatalf("a notice names %s, whose prompt joined the implementer's open message: %q", loopHook.Hook, told)
	}

	hookRowMadeAt(t, runner, pool, againHook, time.Now())
	drainAt(t, runner, 70*time.Minute, "a prompt to the implementer that ends after its message's hour")
	// A notice the drain published is in the listener's record and one it has not is still a row,
	// so the three are not read in the order they were written.
	told = undeliveredNotices(t, pool, publisher)
	if len(told) != 3 || !strings.Contains(strings.Join(told, "\n"), againHook.Hook) {
		t.Fatalf("the architect's notices = %q, want a third, for the implementer's new message, naming %s", told, againHook.Hook)
	}
}

// restartRow is one row of TestARestartedDaemonKeepsARefusedWorkBackInsideItsHour.
type restartRow struct {
	name string
	// after drives the restarted daemon (runner) over its database (pool) and answers why the row
	// failed, or "" for a row that passed.
	after func(t *testing.T, runner *outbox, pool *store.Pool, publisher *holderPublisher, topic string) string
}

// restartRows are the three rows c-r18-daemon names for a daemon that restarts while a refused
// work-back waits inside its hour. The work-back was made ten minutes before the restart, so its
// hour ends fifty minutes after it.
var restartRows = []restartRow{
	{name: "a session holds the role after the restart", after: func(t *testing.T, runner *outbox, pool *store.Pool, publisher *holderPublisher, topic string) string {
		publisher.setAbsent()
		drainAt(t, runner, 2*time.Minute, "the restarted daemon's attempt once a session holds the role")
		if reached := deliveriesTo(publisher, topic); reached != 1 {
			return fmt.Sprintf("the work-back reached the role's session %d times after the restart, want once", reached)
		}
		if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
			return fmt.Sprintf("hook-prompt rows left = %d after the delivery, want the row finished", left)
		}
		if held := heldMessages(t, pool); len(held) != 0 {
			return fmt.Sprintf("the issue holds messages %+v, want none: the work-back was delivered", held)
		}
		return ""
	}},
	{name: "no session holds the role before the hour ends", after: func(t *testing.T, runner *outbox, pool *store.Pool, publisher *holderPublisher, topic string) string {
		drainAt(t, runner, 2*time.Minute, "the restarted daemon's attempt inside the hour")
		if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 {
			return fmt.Sprintf("hook-prompt rows left = %d inside the hour after the restart, want the row waiting", left)
		}
		drainAt(t, runner, 51*time.Minute, "the restarted daemon's attempt past the hour")
		return endedWithTheMessage(t, pool, publisher, topic)
	}},
	{name: "the daemon stays down past the hour", after: func(t *testing.T, runner *outbox, pool *store.Pool, publisher *holderPublisher, topic string) string {
		drainAt(t, runner, 51*time.Minute, "the restarted daemon's first attempt, past the hour")
		return endedWithTheMessage(t, pool, publisher, topic)
	}},
}

// deliveriesTo is how many publishes publisher delivered to topic.
func deliveriesTo(publisher *holderPublisher, topic string) int {
	_, delivered := publisher.snapshot()
	reached := 0
	for _, published := range delivered {
		if published.topic == topic {
			reached++
		}
	}
	return reached
}

// endedWithTheMessage is why a work-back's row did not end undelivered with the message on the
// issue that names it, or "" when it did.
func endedWithTheMessage(t *testing.T, pool *store.Pool, publisher *holderPublisher, topic string) string {
	t.Helper()
	if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
		return fmt.Sprintf("hook-prompt rows left = %d past the hour, want the row ended undelivered", left)
	}
	if reached := deliveriesTo(publisher, topic); reached != 0 {
		return fmt.Sprintf("the work-back reached the role %d times, want none: no session held it", reached)
	}
	held := heldMessages(t, pool)
	if len(held) != 1 || !strings.Contains(held[0].body, firedHook.Prompt) {
		return fmt.Sprintf("the issue holds messages %+v, want one naming the work-back's words", held)
	}
	return ""
}

// A daemon that restarts while a refused work-back waits inside its hour keeps it: the row is the
// work-back's keeper, in the daemon's own database, so the restarted daemon, with a new outbox and
// a new supervisor and only the database carried over, delivers it once to a session that holds
// the role after the restart, ends it at the hour with the message on the issue when none does,
// and ends it at its first attempt when it stays down past the hour. The control runs each row
// again with the restarted daemon given a new database that holds the issue's record and no
// outbox row, which is what a daemon that kept the wait in its process memory alone would leave
// after the restart, and each must fail. At the base the row ends at the first refusal with a log
// line and no message, so each row fails there as well.
func TestARestartedDaemonKeepsARefusedWorkBackInsideItsHour(t *testing.T) {
	t.Parallel()
	for _, row := range restartRows {
		for _, empty := range []bool{false, true} {
			name := row.name
			if empty {
				name = "control: " + row.name + ", over a new empty database"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				runner, _, pool, publisher, topic := unheldHookRunner(t)
				hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-10*time.Minute))
				drainAt(t, runner, 0, "the first daemon's attempt, which the listener refuses")
				if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 || deliveriesTo(publisher, topic) != 0 {
					t.Fatalf("before the restart %d hook-prompt rows wait and %d deliveries were made, want the one refused work-back waiting: a daemon that ends the row at the listener's refusal leaves the restart nothing to keep",
						left, deliveriesTo(publisher, topic))
				}
				// The restart: a new outbox and a new supervisor with no claim, and only the
				// database carried over, or, for the control, a new database.
				restartedPool := pool
				if empty {
					_, _, restartedPool = hookRunner(t, publisher)
				}
				restarted, _ := restartedHookRunner(t, restartedPool, publisher, &outboxDispatch{})
				failure := row.after(t, restarted, restartedPool, publisher, topic)
				switch {
				case !empty && failure != "":
					t.Fatal(failure)
				case empty && failure == "":
					t.Fatal("the row passed with the restarted daemon given a new empty database, so it does not show the database keeps the work-back")
				case empty:
					t.Logf("the control fails, as it must: %s", failure)
				}
			})
		}
	}
}
