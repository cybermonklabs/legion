package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
)

// A hook prompt past its hour, or to a role whose claim failed, is never published, even to a role
// a live session holds: the row ends undelivered with the message on the issue before anything
// wakes or publishes. A parked wake cannot hold the row past its hour either. The control is a row
// one minute old to a held role, which is delivered once.
func TestAHookPromptPastItsHourOrToARoleNotComingIsNeverPublished(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		age       time.Duration
		failed    bool
		delivered int
		says      string
	}{
		{name: "control: a held role inside the hour", age: time.Minute, delivered: 1},
		{name: "a held role past the hour", age: 2 * time.Hour, says: "the hour after the hook fired passed and no session of the implementer took it"},
		{name: "a held role whose claim failed", age: time.Minute, failed: true, says: "the implementer's claim is failed, so no session of it will hold the role again"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			publisher := &holderPublisher{}
			runner, sup, pool := hookRunner(t, publisher)
			if tc.failed {
				failedClaim(t, sup, claim.RoleImplementer)
			}
			hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-tc.age))

			drainAt(t, runner, 0, "drain the hook prompt")

			// The architect's notice of an ended prompt is published to the architect's own topic,
			// so only the implementer's topic is counted here.
			if reached := deliveriesTo(publisher, notify.RoleTopicPrefix+string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))); reached != tc.delivered {
				t.Fatalf("the prompt reached the implementer %d times, want %d, and no publish at all of a prompt that ended", reached, tc.delivered)
			}
			if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
				t.Fatalf("hook-prompt rows left = %d, want the row finished", left)
			}
			held := heldMessages(t, pool)
			if tc.says == "" {
				if len(held) != 0 {
					t.Fatalf("messages on the issue = %+v, want none for a delivered prompt", held)
				}
				return
			}
			if len(held) != 1 {
				t.Fatalf("messages on the issue = %+v, want one", held)
			}
			requireSays(t, held[0].body, tc.says, firedHook.Prompt)
		})
	}
}

// A message row that a post already tried is no open window, though it is held for its retry: a
// prompt that ends then opens a message of its own, so a post that Dispatch committed but whose
// answer was lost, and that the retry finds already posted, leaves no prompt unreported. The
// control is the first message row on its retry backoff, which the row holds before the second
// prompt ends. The head before this rule added the second prompt to the tried row, whose retry
// then found the post made and finished, so no post named the second prompt.
func TestAMessageRowAPostTriedIsNoOpenWindow(t *testing.T) {
	t.Parallel()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	failedClaim(t, sup, claim.RoleImplementer)
	posting := &outboxDispatch{postErr: errors.New("the answer was lost after Dispatch committed the message")}
	runner.dispatch = posting
	hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-time.Minute))
	opened := drainAt(t, runner, 0, "the first prompt's end")
	drainAt(t, runner, time.Hour+time.Second, "the message's first post")
	retried := heldMessages(t, pool)
	if len(posting.messages) != 1 || len(retried) != 1 || !retried[0].due.After(opened.Add(time.Hour+time.Second)) {
		t.Fatalf("UNKNOWN: %d posts and message rows %+v, not one post with the row on its retry backoff, so the row tests nothing", len(posting.messages), retried)
	}
	posting.bodies = append(posting.bodies, posting.messages[0])
	posting.postErr = nil

	hookRowMadeAt(t, runner, pool, loopHook, time.Now())
	drainAt(t, runner, time.Hour+2*time.Second, "the second prompt's end")
	drainAt(t, runner, 2*time.Hour+3*time.Second, "the second message's post")
	named := 0
	for _, body := range posting.messages {
		if strings.Contains(body, loopHook.Prompt) {
			named++
			if strings.Contains(body, firedHook.Prompt) {
				t.Fatalf("the second prompt was posted inside the tried message: %s", body)
			}
		}
	}
	if named != 1 {
		t.Fatalf("posts = %q, want the second prompt named in one post of its own", posting.messages)
	}
}

// heldMessageBarrier stops each NameUndeliveredHookPrompt call that opened a window until release
// is closed, after its own store call returned and while its transaction still holds the
// window's lock, and signals arrived for each such call.
type heldMessageBarrier struct {
	record.Store
	arrived chan struct{}
	release chan struct{}
}

func (s *heldMessageBarrier) NameUndeliveredHookPrompt(ctx context.Context, tx pgx.Tx, issue string, role claim.Role, paragraph string, now time.Time, window time.Duration) (time.Time, bool, error) {
	posts, opened, err := s.Store.NameUndeliveredHookPrompt(ctx, tx, issue, role, paragraph, now, window)
	if err != nil || !opened {
		return posts, opened, err
	}
	s.arrived <- struct{}{}
	select {
	case <-s.release:
	case <-ctx.Done():
		return time.Time{}, false, ctx.Err()
	}
	return posts, opened, nil
}

// Two prompts of one role on one issue that end at the same moment, each in its own transaction,
// open one message window, not two: the first transaction's look for a window holds that window's
// lock until it commits, so the second looks after it and finds the first's row. The first opens
// the window and waits at the barrier; the second must not reach the barrier while it waits.
// The control is the same two finishes one after the other, which open one window.
func TestTwoPromptsThatEndTogetherOpenOneWindow(t *testing.T) {
	t.Parallel()
	for _, concurrent := range []bool{false, true} {
		name := "one after the other"
		if concurrent {
			name = "at the same moment"
		}
		t.Run(name, func(t *testing.T) {
			runner, _, pool := hookRunner(t, &holderPublisher{})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			rows := []record.OutboxRow{
				leasedOutboxRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now())),
				leasedOutboxRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", loopHook, time.Now())),
			}
			cause := &keptRoleMessageUndelivered{why: "the implementer's claim failed", cause: notify.ErrNoHolder}
			if !concurrent {
				for _, row := range rows {
					if err := runner.finishRefused(ctx, row, cause); err != nil {
						t.Fatal(err)
					}
				}
			} else {
				barrier := &heldMessageBarrier{Store: runner.records, arrived: make(chan struct{}, 2), release: make(chan struct{})}
				runner.records = barrier
				results := make(chan error, 2)
				for _, row := range rows {
					go func(row record.OutboxRow) { results <- runner.finishRefused(ctx, row, cause) }(row)
				}
				select {
				case <-barrier.arrived:
				case <-ctx.Done():
					t.Fatal("no finish looked for a window")
				}
				select {
				case <-barrier.arrived:
					close(barrier.release)
					for range 2 {
						<-results
					}
					t.Fatal("both finishes found no window at once: the second did not wait for the first's lock")
				case <-time.After(500 * time.Millisecond):
				}
				close(barrier.release)
				for range 2 {
					if err := <-results; err != nil {
						t.Fatal(err)
					}
				}
			}
			held := heldMessages(t, pool)
			if len(held) != 1 {
				t.Fatalf("the two finishes opened %d message windows, want one", len(held))
			}
			requireSays(t, held[0].body, firedHook.Prompt, loopHook.Prompt)
		})
	}
}

// The message window of one role's undelivered prompts is found by the data its row carries, the
// role and the prompts it names, and never by its words. A window whose body holds other words,
// as a release that words the message otherwise writes it, still takes the role's next prompt,
// and a held message that opens and closes with this release's words but carries no such data is
// no window and is left as it is. Each half is the other's control: a lookup that took any held
// message of the issue would add the prompt to the second, and one by the words would miss the
// first. The head that found the window by its first sentence added the second prompt to the
// message with no data.
func TestAHookPromptWindowIsFoundByItsDataAndNotByItsWords(t *testing.T) {
	t.Parallel()
	runner, _, pool := hookRunner(t, &holderPublisher{})
	ctx := context.Background()
	cause := &keptRoleMessageUndelivered{why: "the implementer's claim failed", cause: notify.ErrNoHolder}
	if err := runner.finishRefused(ctx, leasedOutboxRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", firedHook, time.Now())), cause); err != nil {
		t.Fatal(err)
	}
	var window int64
	if err := pool.QueryRow(ctx, `select id from outbox where issue = 'LEGION-208' and kind = $1`, string(record.OutboxKindDispatchMessage)).Scan(&window); err != nil {
		t.Fatalf("UNKNOWN: the first prompt's end wrote no one message row to test: %v", err)
	}
	const otherWords = "An earlier release worded this message otherwise.\n\nIts closing was other words as well."
	if _, err := pool.Exec(ctx, `update outbox set payload = jsonb_set(payload, '{body}', to_jsonb($2::text)) where id = $1`, window, otherWords); err != nil {
		t.Fatalf("reword the window: %v", err)
	}
	const wordsAlone = "Legion hook prompts to the implementer of LEGION-208 were not delivered. Somebody quoted it.\n\n" +
		"No session of the implementer received these words, and the daemon will not send them again."
	enqueueOutbox(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", record.MessagePost{Body: wordsAlone}, time.Now().Add(time.Hour)))

	if err := runner.finishRefused(ctx, leasedOutboxRow(t, pool, runner.records, mustOutboxRow(t, "LEGION-208", loopHook, time.Now())), cause); err != nil {
		t.Fatal(err)
	}
	held := heldMessages(t, pool)
	if len(held) != 2 {
		t.Fatalf("the outbox holds messages %+v, want the window and the message with no data, and no third", held)
	}
	if held[1].body != wordsAlone {
		t.Fatalf("the message that carries no hook-prompt data took the prompt: its body is now\n\t%s", held[1].body)
	}
	requireSays(t, held[0].body, firedHook.Prompt, loopHook.Prompt)
	var named int
	if err := pool.QueryRow(ctx, `select coalesce(jsonb_array_length(payload->'hook_prompts'->'paragraphs'), 0) from outbox where id = $1`, window).Scan(&named); err != nil || named != 2 {
		t.Fatalf("the window's data names %d prompts (%v), want both", named, err)
	}
}

// A hook prompt to a released role waits past its hour only while a wake of that role can come or
// is under way. While the issue is in the workflow and its tree does not linger, the row's attempt
// wakes the role (wakeForDelivery), and the row waits for the woken session, also past its hour,
// through the launch the wake's resume opened. An issue that has left the workflow gets no wake,
// so its prompt is held to the hour as any other: past it, the row ends undelivered with the
// message on the issue and wakes nothing. Each row is the other's control: a rule that let every
// released role wait would fail the second, which is the head before this bound (its row waited,
// about once a minute, for as long as the issue stayed out), and one that ended every row past
// its hour would fail the first.
func TestAHookPromptToAReleasedRoleWaitsPastItsHourOnlyWhileAWakeCanCome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status string
		ends   bool
	}{
		{name: "the workflow still wakes the role", status: "in_progress"},
		{name: "the issue has left the workflow", status: "backlog", ends: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			publisher := &holderPublisher{}
			runner, sup, pool := runnerIn(t, phase.Implementing, tc.status, publisher, quietLogger())
			holdSlot(t, pool, "LEGION-208")
			topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleImplementer))
			publisher.setAbsent(topic)
			machine := releasedClaim(t, sup, claim.RoleImplementer)
			hookRowMadeAt(t, runner, pool, firedHook, time.Now().Add(-2*time.Hour))

			drainAt(t, runner, 0, "the row's first attempt, past its hour")
			if tc.ends {
				if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 0 {
					t.Fatalf("hook-prompt rows left = %d, want the row ended at its hour: no wake comes to a role of an issue out of the workflow", left)
				}
				if held := machine.Claim(); !held.Released() {
					t.Fatalf("the implementer is %s by %s, want it left released: the issue is out of the workflow", held.State, held.SuspendedBy)
				}
				held := heldMessages(t, pool)
				if len(held) != 1 {
					t.Fatalf("messages on the issue = %+v, want one naming the prompt", held)
				}
				requireSays(t, held[0].body, "the hour after the hook fired passed and no session of the implementer took it", firedHook.Prompt)
				return
			}
			if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 {
				t.Fatalf("hook-prompt rows left = %d after the first attempt, want the row waiting while its released role is woken, past its hour", left)
			}
			if held := machine.Claim(); held.Released() {
				t.Fatalf("UNKNOWN: the implementer is still %s by %s after the first attempt, so the row woke nothing and the wait tests nothing", held.State, held.SuspendedBy)
			}
			drainAt(t, runner, 2*time.Minute, "the row's attempt while the woken role launches")
			t.Logf("the woken implementer is %s at the row's second attempt", machine.View().State)
			if left := rowsOf(t, pool, record.OutboxKindHookPrompt); left != 1 {
				t.Fatalf("hook-prompt rows left = %d, want the row waiting for the woken session past its hour", left)
			}
			if held := heldMessages(t, pool); len(held) != 0 {
				t.Fatalf("messages on the issue = %+v, want none while the role's wake is under way", held)
			}
		})
	}
}
