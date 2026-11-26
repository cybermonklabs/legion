package daemon

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/legion/daemon/internal/claim"
	"github.com/sjawhar/legion/daemon/internal/notify"
	"github.com/sjawhar/legion/daemon/internal/phase"
	"github.com/sjawhar/legion/daemon/internal/record"
	"github.com/sjawhar/legion/daemon/internal/store"
	"github.com/sjawhar/legion/daemon/internal/supervise"
)

// phaseEndedUndeliveredLine is the log line a phase-ended row ends with when no session of its
// role was handed the message (phaseEnded).
const phaseEndedUndeliveredLine = "outbox phase-ended message finished undelivered: no live session holds the role"

// unheldTesterRunner is hookRunner against a listener that keeps nothing for a role no live
// session holds, as the listener answers a publish that names no source session: every publish to
// the tester's topic is refused notify.ErrNoHolder until the test hands the role to a session
// (holderPublisher.setAbsent with no topic). Its log is logs.
func unheldTesterRunner(t *testing.T) (*outbox, *supervisor, *store.Pool, *holderPublisher, string, *syncBuffer) {
	t.Helper()
	publisher := &holderPublisher{}
	runner, sup, pool := hookRunner(t, publisher)
	logs := &syncBuffer{}
	runner.log = slog.New(slog.NewJSONHandler(logs, nil))
	topic := notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", claim.RoleTester))
	publisher.setAbsent(topic)
	return runner, sup, pool, publisher, topic, logs
}

// phaseEndedRowMadeAt enqueues endedByTheWait's row for LEGION-208 as made at made, due now.
func phaseEndedRowMadeAt(t *testing.T, runner *outbox, pool *store.Pool, made time.Time) {
	t.Helper()
	row := mustOutboxRow(t, "LEGION-208", endedByTheWait, time.Now())
	row.CreatedAt = made
	enqueueOutbox(t, pool, runner.records, row)
}

// endedLines is each line of logs that ends a phase-ended row undelivered.
func endedLines(logs *syncBuffer) []string {
	var ended []string
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, phaseEndedUndeliveredLine) {
			ended = append(ended, line)
		}
	}
	return ended
}

// A phase-ended message the listener refuses because no live session holds its role (a listener
// that keeps nothing for a publish naming no source session, as the daemon's phase-ended messages
// name none) waits for a session to hold the role, retried on the row's backoff, and reaches that
// session once it holds the role inside the hour. The row is the message's keeper. At the head
// before this wait the row ends at the first refusal with its log line, and the session that holds
// the role a minute later gets nothing.
func TestAPhaseEndedMessageToAnUnheldRoleWaitsAndReachesItsSessionInsideTheHour(t *testing.T) {
	t.Parallel()
	runner, _, pool, publisher, topic, logs := unheldTesterRunner(t)
	phaseEndedRowMadeAt(t, runner, pool, time.Now().Add(-time.Minute))

	drainAt(t, runner, 0, "the row's first attempt, which the listener refuses")
	if left := rowsOf(t, pool, record.OutboxKindPhaseEnded); left != 1 {
		t.Fatalf("phase-ended rows left = %d after the refusal, want the row waiting for a session to hold the role", left)
	}
	var waited string
	if err := pool.QueryRow(context.Background(), `select last_error from outbox where kind = $1`, string(record.OutboxKindPhaseEnded)).Scan(&waited); err != nil {
		t.Fatalf("read the row's last error: %v", err)
	}
	if !strings.Contains(waited, "waits for a session to hold") {
		t.Fatalf("the row's last error is %q, want the wait for a session to hold the role", waited)
	}
	if ended := endedLines(logs); len(ended) != 0 {
		t.Fatalf("the row logged its end %q, want none inside the hour", ended)
	}

	publisher.setAbsent()
	drainAt(t, runner, 2*time.Minute, "the row's attempt once a session holds the role")
	if reached := deliveriesTo(publisher, topic); reached != 1 {
		t.Fatalf("the message reached the tester's session %d times, want once", reached)
	}
	if left := rowsOf(t, pool, record.OutboxKindPhaseEnded); left != 0 {
		t.Fatalf("phase-ended rows left = %d after the delivery, want the row finished", left)
	}
}

// Once the hour after its row was made has passed with no session of the role handed the message,
// the row ends before anything publishes it, with phaseEndedUndeliveredLine and the reason, and
// no message on the issue: the role's next task from the daemon names its phase anyway. At the
// head before this end the row publishes once more past its hour.
func TestAPhaseEndedMessagePastItsHourEndsWithItsLogLine(t *testing.T) {
	t.Parallel()
	runner, _, pool, publisher, _, logs := unheldTesterRunner(t)
	phaseEndedRowMadeAt(t, runner, pool, time.Now().Add(-time.Hour-time.Minute))

	drainAt(t, runner, 0, "the row's first attempt, past its hour")
	if attempts, delivered := publisher.snapshot(); attempts != 0 || len(delivered) != 0 {
		t.Fatalf("publish attempts = %d with %d delivered, want none for a message past its hour", attempts, len(delivered))
	}
	if left := rowsOf(t, pool, record.OutboxKindPhaseEnded); left != 0 {
		t.Fatalf("phase-ended rows left = %d, want the row ended at its hour", left)
	}
	ended := endedLines(logs)
	if len(ended) != 1 || !strings.Contains(ended[0], "the hour after the phase ended passed") {
		t.Fatalf("the row's end lines are %q, want one naming the hour", ended)
	}
	if held := heldMessages(t, pool); len(held) != 0 {
		t.Fatalf("messages on the issue = %+v, want none for a phase-ended message", held)
	}
}

// keptReleasedRows are the two kinds of kept role message, as a released-role test enqueues and
// reads them: a hook prompt to the implementer of an issue in implementing, which ends with the
// message on the issue, and a phase-ended message to the tester of an issue in testing, which ends
// with its log line and no message on the issue.
var keptReleasedRows = []struct {
	kind    string
	role    claim.Role
	at      phase.Phase
	rows    record.OutboxKind
	enqueue func(t *testing.T, runner *outbox, pool *store.Pool, made time.Time)
	// ended fails unless the row's end was told as its kind tells it; waiting fails unless no end
	// was told.
	ended, waiting func(t *testing.T, pool *store.Pool, logs *syncBuffer)
}{
	{
		kind: "a hook prompt", role: claim.RoleImplementer, at: phase.Implementing, rows: record.OutboxKindHookPrompt,
		enqueue: func(t *testing.T, runner *outbox, pool *store.Pool, made time.Time) {
			hookRowMadeAt(t, runner, pool, firedHook, made)
		},
		ended: func(t *testing.T, pool *store.Pool, _ *syncBuffer) {
			held := heldMessages(t, pool)
			if len(held) != 1 {
				t.Fatalf("messages on the issue = %+v, want one naming the prompt", held)
			}
			requireSays(t, held[0].body, "the hour after the hook fired passed and no session of the implementer took it", firedHook.Prompt)
		},
		waiting: func(t *testing.T, pool *store.Pool, _ *syncBuffer) {
			if held := heldMessages(t, pool); len(held) != 0 {
				t.Fatalf("messages on the issue = %+v, want none while the role's wake is under way", held)
			}
		},
	},
	{
		kind: "a phase-ended message", role: claim.RoleTester, at: phase.Testing, rows: record.OutboxKindPhaseEnded,
		enqueue: phaseEndedRowMadeAt,
		ended: func(t *testing.T, _ *store.Pool, logs *syncBuffer) {
			if ended := endedLines(logs); len(ended) != 1 {
				t.Fatalf("the row's end lines are %q, want one", ended)
			}
		},
		waiting: func(t *testing.T, _ *store.Pool, logs *syncBuffer) {
			if ended := endedLines(logs); len(ended) != 0 {
				t.Fatalf("the row logged its end %q, want none while the role's wake is under way", ended)
			}
		},
	},
}

// A kept role message to a released role waits past its hour only while a wake of that role can
// come or is under way (wakeComing). While the issue is in the workflow and its tree does not
// linger, the row's attempt wakes the role (deliveryWake), and the row waits for the woken session,
// also past its hour, through the launch the wake's resume opened. An issue that has left the
// workflow gets no wake, so its row is held to the hour as any other: past it, the row ends as its
// kind ends, publishes nothing and wakes nothing. Each row is the other's control: a rule that let
// every released role wait would fail the second, and one that ended every row past its hour would
// fail the first.
func TestAKeptRoleMessageToAReleasedRoleWaitsPastItsHourOnlyWhileAWakeCanCome(t *testing.T) {
	t.Parallel()
	for _, message := range keptReleasedRows {
		for _, tc := range []struct {
			name   string
			status string
			ends   bool
		}{
			{name: "the workflow still wakes the role", status: "in_progress"},
			{name: "the issue has left the workflow", status: "backlog", ends: true},
		} {
			t.Run(message.kind+": "+tc.name, func(t *testing.T) {
				t.Parallel()
				publisher := &holderPublisher{}
				logs := &syncBuffer{}
				runner, sup, pool := runnerIn(t, message.at, tc.status, publisher, slog.New(slog.NewJSONHandler(logs, nil)))
				holdSlot(t, pool, "LEGION-208")
				publisher.setAbsent(notify.RoleTopicPrefix + string(mustClaimToken(t, "LEGION-208", message.role)))
				machine := releasedClaim(t, sup, message.role)
				message.enqueue(t, runner, pool, time.Now().Add(-2*time.Hour))

				drainAt(t, runner, 0, "the row's first attempt, past its hour")
				if tc.ends {
					if left := rowsOf(t, pool, message.rows); left != 0 {
						t.Fatalf("%s rows left = %d, want the row ended at its hour: no wake comes to a role of an issue out of the workflow", message.rows, left)
					}
					if attempts, _ := publisher.snapshot(); attempts != 0 {
						t.Fatalf("publish attempts = %d, want none for a message past its hour", attempts)
					}
					if held := machine.Claim(); !held.Released() {
						t.Fatalf("the %s is %s by %s, want it left released: the issue is out of the workflow", message.role, held.State, held.SuspendedBy)
					}
					message.ended(t, pool, logs)
					return
				}
				if left := rowsOf(t, pool, message.rows); left != 1 {
					t.Fatalf("%s rows left = %d after the first attempt, want the row waiting while its released role is woken, past its hour", message.rows, left)
				}
				if held := machine.Claim(); held.Released() {
					t.Fatalf("UNKNOWN: the %s is still %s by %s after the first attempt, so the row woke nothing and the wait tests nothing", message.role, held.State, held.SuspendedBy)
				}
				drainAt(t, runner, 2*time.Minute, "the row's attempt while the woken role launches")
				t.Logf("the woken %s is %s at the row's second attempt", message.role, machine.View().State)
				if left := rowsOf(t, pool, message.rows); left != 1 {
					t.Fatalf("%s rows left = %d, want the row waiting for the woken session past its hour", message.rows, left)
				}
				message.waiting(t, pool, logs)
			})
		}
	}
}

// A role whose claim failed or was retired has no session coming back for it, so its phase-ended
// row ends at once, inside the hour, before anything publishes, with its log line naming that end.
// At the head before this end the row publishes to the role once first.
func TestAPhaseEndedMessageToARoleThatIsNotComingEndsAtOnce(t *testing.T) {
	t.Parallel()
	for _, state := range []supervise.ClaimState{supervise.StateFailed, supervise.StateRetired} {
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			runner, sup, pool, publisher, _, logs := unheldTesterRunner(t)
			if _, _, err := sup.Create(context.Background(), supervise.Claim{Token: mustClaimToken(t, "LEGION-208", claim.RoleTester),
				Project: "legion", Tree: "LEGION-208", Issue: "LEGION-208", Role: claim.RoleTester, Generation: 1, ServingGeneration: 1,
				State: state}, ""); err != nil {
				t.Fatalf("create the %s tester's claim: %v", state, err)
			}
			phaseEndedRowMadeAt(t, runner, pool, time.Now().Add(-time.Minute))

			drainAt(t, runner, 0, "the row's first attempt")
			if attempts, _ := publisher.snapshot(); attempts != 0 {
				t.Fatalf("publish attempts = %d, want none to a role whose claim is %s", attempts, state)
			}
			if left := rowsOf(t, pool, record.OutboxKindPhaseEnded); left != 0 {
				t.Fatalf("phase-ended rows left = %d, want the row ended at once", left)
			}
			ended := endedLines(logs)
			if len(ended) != 1 || !strings.Contains(ended[0], "the role's claim is "+string(state)) {
				t.Fatalf("the row's end lines are %q, want one naming the %s claim", ended, state)
			}
		})
	}
}
