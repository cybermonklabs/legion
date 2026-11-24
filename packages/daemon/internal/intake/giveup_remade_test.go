package intake

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/sjawhar/legion/daemon/internal/testwait"
)

// A durable made again at its stored position checks its start on its own first message, which it
// fetches and hands back (checkStart), so that message reaches Run as its second delivery, and the
// handling notes the redelivery (noteRedelivery) before it runs. Each row deletes the Dispatch
// durable after CAPTURE-4 was applied (its stored position) and CAPTURE-3 failed after it:
//
//   - with the handler now succeeding, the remade durable's checked first message, CAPTURE-4, is
//     dropped as the duplicate it is, and CAPTURE-3, whose open failure the remade durable read
//     (remadeFailures), is applied; neither leaves an open failure, and nothing is given up;
//   - control: with the handler still failing, CAPTURE-3's open failure stays open and is written
//     again by the remade durable's delivery, so the clear in the first row is its success's, and
//     the records this test reads would show an open failure that was left.
//
// Both rows also show the checked message came twice: once to the check, before Run started, and
// again to Run.
func TestARemadeDurablesFirstMessagesLeaveNoOpenFailureOnceHandled(t *testing.T) {
	for _, row := range []struct {
		name     string
		succeeds bool
	}{
		{name: "the handler succeeds once the durable is made again", succeeds: true},
		{name: "control: the handler still fails"},
	} {
		t.Run(row.name, func(t *testing.T) {
			pool := migratedPool(t)
			createWrites(t, pool)
			js, stream := testJetStream(t)
			spec := consumerSpec(&lockedBuffer{})
			var failing atomic.Bool
			failing.Store(true)
			handler := handlerFunc(func(ctx context.Context, tx pgx.Tx, fact Fact) (Result, error) {
				issue, ok := fact.(DispatchIssue)
				if ok && issue.Key == "CAPTURE-3" && failing.Load() {
					return Result{}, errAlwaysFails
				}
				name := "fact"
				if ok {
					name = issue.Key
				}
				_, err := tx.Exec(ctx, `insert into intake_test_writes (handler) values ($1)`, name)
				return Result{}, err
			})
			dispatch := dispatchConsumerName(spec.Project)

			consumers, err := OpenConsumers(context.Background(), js, pool, spec)
			if err != nil {
				t.Fatalf("OpenConsumers: %v", err)
			}
			stop := runConsumers(t, consumers, handler)
			applied := publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-4.issue.created", capturedIssueCreatedEnvelope(t))
			testwait.Eventually(t, "CAPTURE-4 applied", func() bool { return slices.Equal(writeHandlers(t, pool), []string{"CAPTURE-4"}) })
			failed := publish(t, js, "notifications.dispatch.issue.CAPTURE.CAPTURE-3.issue.updated", capturedIssueUpdatedEnvelope(t))
			waitOpenFailures(t, pool, dispatch, failed.Sequence)
			stop()
			before, _ := intakeRecordOf(t, pool, dispatch, failed.Sequence)

			if err := stream.DeleteConsumer(context.Background(), dispatch); err != nil {
				t.Fatalf("delete the Dispatch durable: %v", err)
			}
			failing.Store(!row.succeeds)
			if consumers, err = OpenConsumers(context.Background(), js, pool, spec); err != nil {
				t.Fatalf("OpenConsumers with the Dispatch durable missing: %v", err)
			}
			// The check fetched CAPTURE-4 and handed it back: nats-server keeps a handed-back
			// message among those awaiting an acknowledgement until it delivers it again.
			checked := consumerInfo(t, stream, dispatch)
			if checked.Delivered.Consumer != 1 || checked.Delivered.Stream != applied.Sequence {
				t.Fatalf("the remade durable delivered %d (through stream sequence %d) before Run, want the check's one fetch of CAPTURE-4 at %d",
					checked.Delivered.Consumer, checked.Delivered.Stream, applied.Sequence)
			}
			defer runConsumers(t, consumers, handler)()

			if row.succeeds {
				assertAcknowledgedThrough(t, stream, dispatch, failed)
				testwait.Eventually(t, "every open failure of the remade durable cleared", func() bool { return len(intakeRecords(t, pool)) == 0 })
				if got := writeHandlers(t, pool); !slices.Equal(got, []string{"CAPTURE-4", "CAPTURE-3"}) {
					t.Fatalf("applied %q, want CAPTURE-4 once, before the durable went missing, and CAPTURE-3 once after", got)
				}
			} else {
				testwait.Eventually(t, "the remade durable's delivery of CAPTURE-3 noted on its open failure", func() bool {
					f, ok := intakeRecordOf(t, pool, dispatch, failed.Sequence)
					return ok && !f.GivenUp() && f.UpdatedAt.After(before.UpdatedAt)
				})
				waitAckFloor(t, stream, dispatch, applied.Sequence)
				if _, ok := intakeRecordOf(t, pool, dispatch, applied.Sequence); ok {
					t.Fatalf("the checked first message, CAPTURE-4, left a record, want none: it was dropped as a duplicate")
				}
			}
			if delivered := consumerInfo(t, stream, dispatch).Delivered.Consumer; delivered < 3 {
				t.Fatalf("the remade durable delivered %d messages, want CAPTURE-4 twice (the check's fetch and Run's) and CAPTURE-3 at least once", delivered)
			}
		})
	}
}
