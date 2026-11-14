package bus

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/sjawhar/envoy/internal/contracts"
)

// loweredStartLine is the INFO line of a caller's start that create lowered to the session's resume
// record. A production check reads it by this text, so the tests name it whole.
const loweredStartLine = "session consumer created at its resume record's floor, below the start the caller asked"

// A start a restart left pending reads no resume record (create): it is the floor of the consumer
// that restart deleted, which is newer than any record, so a missing consumer is created at it and
// not at an older record's floor plus one, which would hand the session again every message it
// acked since that record. The create writes no INFO line, because nothing lowered its start.
//
// The session's record is written by a delete after one acked message; a consumer created again
// from it then takes and acks three more, and holds a fourth unacked, so a restart at its floor
// deletes it and leaves the fourth's sequence pending when its create faults.
func TestAPendingStartIsNotLoweredToAnOlderResumeRecord(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_pending_above_record"
	agent := contracts.AgentSubject(session)
	filters := []string{agent}
	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first := publishStored(t, owner, agent, "first")
	pull := pullSession(t, consumers, session)
	if err := requireNext(t, pull, first, "the first message on the session's agent subject").Ack(); err != nil {
		t.Fatalf("ack %d: %v", first, err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, first)
	recordFloor, err := consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}

	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure from the record: %v", err)
	}
	pull = pullSession(t, consumers, session)
	var acked uint64
	for i := range 3 {
		acked = publishStored(t, owner, agent, fmt.Sprintf("acked %d", i))
		if err := requireNext(t, pull, acked, "the messages stored after the record's floor, in order").Ack(); err != nil {
			t.Fatalf("ack %d: %v", acked, err)
		}
	}
	waitForAckFloor(t, consumers, session, acked)
	held := publishStored(t, owner, agent, "held")
	requireNext(t, pull, held, "the message the session holds unacked")
	pull.Stop()
	waitForAckPending(t, consumers, session, 1)

	consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
	if _, err := consumers.RestartAtFloor(ctx, session, filters); err == nil {
		t.Fatal("RestartAtFloor whose create faulted reported no error")
	}
	consumers.SetCreateFault(nil)

	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure at the pending start: %v", err)
	}
	if !start.Created || start.StartSeq != held {
		t.Errorf("Ensure with a pending start of %d and a resume record floor of %d = %+v; want the consumer created at the pending start %d, not at the record's floor plus one", held, recordFloor, start, held)
	}
	requireConsumerStart(t, consumers, session, held, "the pending start, the floor of the consumer the restart deleted plus one")
	if lines := sessionLines(t, logs, loweredStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for a create at a pending start; want no line: no record lowered it", lines)
	}
}

// The INFO line of the record rule is written for a caller's start the record lowered, and only for
// one (create): a create with no start of its own starts at the record's floor plus one and lowers
// nothing, and a caller's start at that floor plus one is not above it, so neither writes the line.
// A caller's start inside the stream above that floor plus one is lowered, not only one past the
// stream's end, and writes one line naming the caller's start, the record's floor and the stream's
// identity; the session is then handed every message after the floor, the ones its start said it
// already held included.
func TestTheRecordRuleLogsEachStartItLoweredAndNoOther(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_record_rule_lines"
	agent := contracts.AgentSubject(session)
	filters := []string{agent}
	for i := range 3 {
		publishStored(t, owner, contracts.AgentSubject("ses_record_rule_lines_bystander"), fmt.Sprintf("bystander %d", i))
	}
	if _, err := consumers.Ensure(ctx, session, filters, 0); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	seq := publishStored(t, owner, agent, "taken")
	pull := pullSession(t, consumers, session)
	if err := requireNext(t, pull, seq, "the session's first message").Ack(); err != nil {
		t.Fatalf("ack %d: %v", seq, err)
	}
	pull.Stop()
	waitForAckFloor(t, consumers, session, seq)

	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if start, err := consumers.Ensure(ctx, session, filters, 0); err != nil || start.StartSeq != floor+1 {
		t.Fatalf("Ensure with no start after the delete = %+v, %v; want the consumer created at %d, the record's floor plus one", start, err, floor+1)
	}
	if lines := sessionLines(t, logs, loweredStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for a create with no start of its own; want no line: no caller's start was lowered", lines)
	}

	if floor, err = consumers.Delete(ctx, session, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if start, err := consumers.Ensure(ctx, session, filters, floor+1); err != nil || start.StartSeq != floor+1 {
		t.Fatalf("Ensure at the record's floor plus one = %+v, %v; want the consumer created at %d", start, err, floor+1)
	}
	if lines := sessionLines(t, logs, loweredStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for a caller's start at the record's floor plus one; want no line: the record lowered nothing", lines)
	}

	if floor, err = consumers.Delete(ctx, session, nil); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var away []uint64
	for i := range 3 {
		away = append(away, publishStored(t, owner, agent, fmt.Sprintf("away %d", i)))
	}
	// A resume point at the second message stored while the session was away: the caller's start
	// is the third, inside the stream and above the record's floor plus one.
	asked := away[2]
	start, err := consumers.Ensure(ctx, session, filters, asked)
	if err != nil || start.StartSeq != floor+1 {
		t.Fatalf("Ensure at %d, inside the stream = %+v, %v; want the consumer created at %d, the record's floor plus one", asked, start, err, floor+1)
	}
	requireOneLine(t, logs, "INFO", loweredStartLine, session, map[string]any{
		"asked_start":  asked,
		"record_floor": floor,
		"stream_id":    streamIDOf(t, consumers),
	})
	pull = pullSession(t, consumers, session)
	for _, seq := range away {
		requireNext(t, pull, seq, "every message after the record's floor, the ones the caller's start said the session held included")
	}
}
