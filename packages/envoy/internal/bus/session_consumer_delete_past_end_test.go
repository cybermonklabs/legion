package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// floorPastEndLine is the WARN line of a resume floor past the stream's end, which the record
// holds at the sequence before the stream's first (recordResumeFloorAt). The tests name it whole,
// as they name the lines of a start (cmd/listener/session_resume_past_end_test.go).
const floorPastEndLine = "session resume floor past the stream's end, recorded at the sequence before the stream's first"

// deletePath is one path that takes a session's consumer away and records the session's resume
// floor before it does (SessionConsumers.Delete): a deregistration's delete, the sweep's delete
// from the consumer list, and the delete that finds no consumer and records the pending start a
// restart left (recordDroppedStart). It returns the floor that delete answered.
type deletePath func(t *testing.T, consumers *SessionConsumers, session string, filters []string) uint64

// createAgain is one of the two ways the session's next consumer is created once a delete has
// recorded a floor for it (SessionConsumers.Ensure): at a caller's start past the stream's end,
// which a client that holds a resume point past the end asks for (cmd/listener consumerFor), and
// with no start of its own, which a registration that carries no resume point gives. The record
// decides both - it lowers the caller's start to its floor plus one, and it is the whole of the
// start with no caller's start - so both are a test of the floor the delete recorded.
type createAgain struct {
	name string
	// start is what Ensure is passed, built from the stream's last sequence after the delete.
	start func(lastSeq uint64) uint64
	// lowered says the record rule lowers that start to the record's floor plus one, which the
	// create logs in one INFO line; a create with no start of its own lowers nothing.
	lowered bool
}

// createdAgain is the two ways, which every case below runs as a subtest of its own, on a stream
// of its own: a subtest takes a JetStream account no other test used (testnats.URL), so each
// builds the consumer, the delete and the create again from an empty stream.
var createdAgain = []createAgain{
	{
		name:    "created again at a start past the stream's end",
		start:   func(lastSeq uint64) uint64 { return lastSeq + pastTheEnd + 1 },
		lowered: true,
	},
	{
		name:  "created again with no start of its own",
		start: func(uint64) uint64 { return 0 },
	},
}

// A delete of a session consumer whose floor is past the stream's end records the sequence before
// the stream's first (Delete, recordResumeFloorAt), and the session's next consumer starts at the
// stream's first sequence and is handed every message the stream holds on its filters.
//
// The consumer is one an earlier build created past the end, before create reset every start past
// the end (createAsAnEarlierBuild), and the messages this stores on the session's agent subject
// after that create are below its old start, so it would hand over none of them. Its floor, the
// sequence before that start on nats-server v2.15.1 (ackFloor), names no message the stream holds:
// the messages now at the sequences up to the stream's last were stored after the consumer's start
// was fixed and were never handed to it. A record of the stream's last sequence would start the
// next consumer after them and skip every one; the sequence before the stream's first starts it at
// the stream's first sequence, where create's reset of a start past the end and the relay's
// restart put such a consumer too.
//
// So this requires, of each delete path and for each way the next consumer is created: the floor
// the delete answered and the floor its record holds are the stream's first sequence minus one,
// read from the stream; one WARN line for the floor, naming the floor asked (floor), the floor
// recorded (recorded_floor), the stream's last sequence (stream_last_seq) and the stream's
// identity; and the consumer created again at the stream's first sequence, which hands over the
// messages stored below the old start, in stream order, and then one stored after the create.
func deletePastEndRecordsTheSequenceBeforeTheStreamsFirst(t *testing.T, session string, path deletePath) {
	t.Helper()
	for _, again := range createdAgain {
		t.Run(again.name, func(t *testing.T) {
			owner, consumers, logs := sessionConsumersLogging(t)
			agent := contracts.LegacyAgentSubject(session)
			filters := []string{agent}
			// Another session's messages, so the stream's first sequence is below every message
			// this case stores for the session itself.
			for i := range 3 {
				publishStored(t, owner, contracts.LegacyAgentSubject(session+"_bystander"), fmt.Sprintf("bystander %d", i))
			}
			id := streamIDOf(t, consumers)
			lastSeq := createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)

			// The messages stored on the session's agent subject after that create, below the
			// consumer's old start: the consumer as it stands hands over none of them.
			var held []uint64
			for i := range 2 {
				seq := publishStored(t, owner, agent, fmt.Sprintf("below the old start %d", i))
				if want := lastSeq + uint64(i) + 1; seq != want {
					t.Fatalf("message %d on the agent subject of %s was stored at %d, want %d: something else was stored since the stream was read", i, session, seq, want)
				}
				held = append(held, seq)
			}
			end := streamStateOf(t, consumers)
			if end.lastSeq != lastSeq+2 {
				t.Fatalf("the stream's last sequence before the delete is %d, want %d, the second message stored below the old start: something else was stored", end.lastSeq, lastSeq+2)
			}

			floor := path(t, consumers, session, filters)

			// The floor the delete read is the sequence before the consumer's old start, which
			// the server reports as its ack floor and a restart leaves as its pending start.
			requireFloorBeforeTheStreamsFirst(t, consumers, logs, session, id, floor, lastSeq+pastTheEnd-1, end)
			createdAtTheStreamsFirstSequence(t, again, owner, consumers, logs, session, id, floor, held)
		})
	}
}

// requireFloorBeforeTheStreamsFirst requires answered, the floor the delete answered, and the floor
// the session's resume record holds to be the sequence before the stream's first, as end read the
// stream just before the delete, and the delete to have written one WARN line for the floor asked,
// which is past the stream's end. It fails the test without stopping it, so the case goes on to
// show what the next consumer is handed.
func requireFloorBeforeTheStreamsFirst(t *testing.T, consumers *SessionConsumers, logs *capturedLogs, session, id string, answered, asked uint64, end sessionStreamState) {
	t.Helper()
	want := end.firstStart() - 1
	if answered != want {
		t.Errorf("the delete of a consumer whose floor %d is past the stream's end answered the floor %d, want %d, the sequence before the stream's first %d, not the stream's last sequence %d",
			asked, answered, want, end.firstStart(), end.lastSeq)
	}
	if floor, recorded, err := consumers.resumeFloor(context.Background(), session, id); err != nil || !recorded || floor != want {
		t.Errorf("the session's resume record = floor %d, recorded %t, %v; want the floor %d of stream %s, the sequence before the stream's first, not the stream's last sequence %d",
			floor, recorded, err, want, id, end.lastSeq)
	}
	requireOneLine(t, logs, "WARN", floorPastEndLine, session, map[string]any{
		"floor":           asked,
		"recorded_floor":  want,
		"stream_last_seq": end.lastSeq,
		"stream_id":       id,
	})
}

// createdAtTheStreamsFirstSequence creates session's consumer again the way again asks (Ensure),
// once a delete has recorded recordFloor for it, and requires it to start at the stream's first
// sequence: a caller's start past the end is lowered to recordFloor plus one by the record rule,
// which writes one INFO line, and a create with no start of its own starts there with no line at
// all. Neither is a clamp, since recordFloor plus one is the stream's first sequence, so no create
// writes the clamp's line. The consumer must then hand over held, the messages stored below the
// old start, in stream order, and then one message stored after the create.
func createdAtTheStreamsFirstSequence(t *testing.T, again createAgain, owner *Client, consumers *SessionConsumers, logs *capturedLogs, session, id string, recordFloor uint64, held []uint64) {
	t.Helper()
	ctx := context.Background()
	agent := contracts.LegacyAgentSubject(session)
	state := streamStateOf(t, consumers)
	first := state.firstStart()
	asked := again.start(state.lastSeq)

	start, err := consumers.Ensure(ctx, session, []string{agent}, asked)
	if err != nil {
		t.Fatalf("Ensure after the delete, asked for the start %d: %v", asked, err)
	}
	if want := (SessionConsumerStart{Name: fixedName(t, session), Created: true, StartSeq: first, StreamID: id, AckFloor: first - 1}); start != want {
		t.Errorf("Ensure after the delete, asked for the start %d = %+v; want %+v, the consumer created at the stream's first sequence, the floor %d the delete recorded plus one", asked, start, want, recordFloor)
	}
	requireConsumerStart(t, consumers, session, first, "the stream's first sequence, the floor the delete recorded plus one")
	if again.lowered {
		requireOneLine(t, logs, "INFO", loweredStartLine, session, map[string]any{
			"asked_start":  asked,
			"record_floor": recordFloor,
			"stream_id":    id,
		})
	} else if lines := sessionLines(t, logs, loweredStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for a create with no start of its own; want no line: no caller's start was lowered", lines)
	}
	if lines := sessionLines(t, logs, clampedStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for the create from the record; want no clamp line: the floor the delete recorded plus one is the stream's first sequence, which is not past the stream's end", lines)
	}

	written := publishStored(t, owner, agent, "written after the create")
	pull := pullSession(t, consumers, session)
	for _, seq := range held {
		requireNext(t, pull, seq,
			"a message stored below the old start of the consumer the delete took; a consumer created at the sequence after a floor the stream's last sequence would have held skips it")
	}
	requireNext(t, pull, written, "the message stored after the create")
}

// A deregistration's delete of a consumer past the stream's end records the sequence before the
// stream's first (deletePastEndRecordsTheSequenceBeforeTheStreamsFirst): the listener deletes a
// session's consumer with the registration it removed, handing Delete no info of its own, so the
// delete reads the consumer itself and records the floor that read holds
// (cmd/listener sessionDelivery.deleteConsumer).
func TestADeregistrationsDeleteOfAConsumerPastTheEndRecordsTheSequenceBeforeTheStreamsFirst(t *testing.T) {
	deletePastEndRecordsTheSequenceBeforeTheStreamsFirst(t, "ses_deleted_past_end", func(t *testing.T, consumers *SessionConsumers, session string, _ []string) uint64 {
		t.Helper()
		floor, err := consumers.Delete(context.Background(), session, nil)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		return floor
	})
}

// The sweep's delete of a consumer past the stream's end records the sequence before the stream's
// first (deletePastEndRecordsTheSequenceBeforeTheStreamsFirst): a sweep of the sessions the
// registry no longer holds reads every session consumer in a few paged requests
// (ListSessionConsumers) and hands Delete the info it listed, which saves that delete an info read
// of its own, as cmd/listener's sweep calls it (cmd/listener session_consumer_sweep.go, which
// passes the listed info to sessionDelivery.deleteConsumer). The floor recorded is the listed
// info's, and it is past the stream's end as the consumer's own read of it would be.
func TestTheSweepsDeleteOfAConsumerPastTheEndRecordsTheSequenceBeforeTheStreamsFirst(t *testing.T) {
	deletePastEndRecordsTheSequenceBeforeTheStreamsFirst(t, "ses_swept_past_end", func(t *testing.T, consumers *SessionConsumers, session string, _ []string) uint64 {
		t.Helper()
		ctx := context.Background()
		listed, err := consumers.ListSessionConsumers(ctx)
		if err != nil {
			t.Fatalf("list the session consumers, as the sweep reads them: %v", err)
		}
		if listed[session] == nil {
			t.Fatalf("the list holds no consumer for %s (%d listed), so the sweep would delete none", session, len(listed))
		}
		floor, err := consumers.Delete(ctx, session, listed[session])
		if err != nil {
			t.Fatalf("delete from the listed info: %v", err)
		}
		return floor
	})
}

// A delete that finds no consumer, and records the pending start a restart past the stream's end
// left, records the sequence before the stream's first
// (deletePastEndRecordsTheSequenceBeforeTheStreamsFirst, recordDroppedStart). The restart of a
// consumer past the end does not keep it (keepAtFloor): it leaves the sequence after the floor it
// read as the consumer's pending start, deletes the consumer and then fails its create
// (SetCreateFault), which is the state a listener killed between the two leaves. That pending
// start is past the stream's end, so the delete that reaches the session next records the sequence
// before the stream's first for the sequence before it, and drops the start, which would otherwise
// have the next create undo the delete.
func TestADroppedPendingStartPastTheEndRecordsTheSequenceBeforeTheStreamsFirst(t *testing.T) {
	deletePastEndRecordsTheSequenceBeforeTheStreamsFirst(t, "ses_dropped_past_end", func(t *testing.T, consumers *SessionConsumers, session string, filters []string) uint64 {
		t.Helper()
		ctx := context.Background()
		name, _ := SessionConsumerName(session)
		consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
		_, err := consumers.RestartAtFloor(ctx, session, filters)
		consumers.SetCreateFault(nil)
		if err == nil {
			t.Fatal("RestartAtFloor whose create faulted reported no error")
		}
		if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
			t.Fatalf("the consumer's info after the faulted restart = %v, want ErrSessionConsumerNotFound: the restart deleted it", err)
		}
		pending, ok := consumers.pendingStart(name)
		if !ok {
			t.Fatal("the restart whose create faulted left no pending start, so the delete below would record nothing")
		}
		floor, err := consumers.Delete(ctx, session, nil)
		if err != nil {
			t.Fatalf("the delete that found no consumer: %v", err)
		}
		if start, ok := consumers.pendingStart(name); ok {
			t.Errorf("the pending start after the delete is %d, want none: the next create would undo the delete", start)
		}
		t.Logf("the restart left the pending start %d, past the stream's end, and the delete that found no consumer recorded the floor %d for the sequence before it", pending, floor)
		return floor
	})
}

// A delete of the consumer a one-copy stream's whole loss left unreconciled records the sequence
// before the stream's first, as a delete of one an earlier build created past the end does
// (deletePastEndRecordsTheSequenceBeforeTheStreamsFirst), and the session's next consumer starts at
// the stream's first sequence and is handed the messages the stream stored after the loss.
//
// The consumer of the session is created at N + 1 by Ensure, on a one-copy stream holding N = 20
// messages on a subject it does not follow, and delivers nothing. With the server stopped, the
// stream's first message block is cut to nothing and its index removed, as the loss cuts make a
// whole loss (testnats.Server.CutStream), and the restarted server holds no message. nats-server
// v2.15.1 does not reconcile such a consumer: the reconciled state is all 0, which the consumer's
// file store reads as state not loaded yet and writes the old state back over, so the consumer
// still reports an ack floor of N, above the stream's last sequence, while the stream carries the
// identity it had before the loss (ackFloor, RestartAtFloor).
//
// The stream then stores K = 2 messages on the session's agent subject, at 1 and 2, which that
// consumer would never hand over. Its delete must record the sequence before the stream's first,
// 0, and not the stream's last sequence, 2, which would start the next consumer at 3 and skip both
// of them; the consumer created again, at a start past the end or with no start of its own, must
// start at 1 and hand over both and then the next message stored.
func TestADeleteOfTheConsumerAWholeLossLeftUnreconciledRecordsTheSequenceBeforeTheStreamsFirst(t *testing.T) {
	for _, again := range createdAgain {
		t.Run(again.name, func(t *testing.T) {
			srv, uri := testnats.Start(t)
			owner, err := ConnectOwningStream([]string{uri}, WithReplicas(1))
			if err != nil {
				t.Fatalf("connect the stream's owner: %v", err)
			}
			t.Cleanup(owner.Close)
			logs := &capturedLogs{}
			consumers, err := ConnectSessionConsumers([]string{uri}, 0, slog.New(slog.NewJSONHandler(logs, nil)))
			if err != nil {
				t.Fatalf("connect session consumers: %v", err)
			}
			t.Cleanup(consumers.Close)
			ctx := context.Background()
			// before is how many messages the stream holds when the consumer is created (N).
			const before = 20
			const session = "ses_delete_whole_loss"
			agent := contracts.LegacyAgentSubject(session)
			filters := []string{agent}

			for i := range before {
				publishStored(t, owner, contracts.LegacyAgentSubject(session+"_bystander"), fmt.Sprintf("before %02d", i))
			}
			id := streamIDOf(t, consumers)
			start, err := consumers.Ensure(ctx, session, filters, 0)
			if err != nil {
				t.Fatalf("ensure: %v", err)
			}
			if !start.Created || start.StartSeq != before+1 {
				t.Fatalf("Ensure of a session the stream holds nothing for = %+v, want the consumer created at %d, the sequence after the stream's last", start, before+1)
			}
			// nats-server v2.15.1 reports the floor of a consumer that has acked nothing as the
			// sequence before its start, N.
			waitForAckFloor(t, consumers, session, before)
			waitForAckPending(t, consumers, session, 0)

			srv.CutStream(t, Stream, func(int64) int64 { return 0 })

			testwait.Eventually(t, "the stream's owner and the session consumers to reconnect", func() bool {
				return owner.Connected() && consumers.client.Connected()
			})
			var state sessionStreamState
			if !testwait.Holds(testwait.Bounded(t, time.Minute, time.Second), 100*time.Millisecond, func() bool {
				state, err = consumers.streamState(ctx)
				return err == nil
			}) {
				t.Fatalf("read the stream after the restart: %v", err)
			}
			if state.lastSeq != 0 {
				t.Fatalf("the stream's last sequence after the whole block was cut is %d, want 0", state.lastSeq)
			}
			if state.id != id {
				t.Fatalf("the stream's identity after the cut is %q, want %q, the one it carried before: the stream is the one the consumer's floor was counted on", state.id, id)
			}

			held := []uint64{publishStored(t, owner, agent, "held 1"), publishStored(t, owner, agent, "held 2")}
			if held[0] != 1 || held[1] != 2 {
				t.Fatalf("the messages stored after the cut took sequences %v, want 1 and 2", held)
			}
			info, err := consumers.Info(ctx, session)
			if err != nil {
				t.Fatalf("info after the cut: %v", err)
			}
			t.Logf("after the cut, with the stream's last sequence at %d, the server reports the ack floor as stream %d consumer %d, the last delivery as %d, %d awaiting an ack and the start sequence as %d; the floor this package reads from that is %d",
				held[1], info.AckFloor.Stream, info.AckFloor.Consumer, info.Delivered.Stream, info.NumAckPending, info.Config.OptStartSeq, ackFloor(info))
			if info.AckFloor.Stream <= held[1] {
				t.Fatalf("the server reports the consumer's ack floor as %d, want it above the stream's last sequence %d: the consumer this case is for is the one nats-server v2.15.1 leaves unreconciled at the floor %d it had before the loss", info.AckFloor.Stream, held[1], before)
			}
			end := streamStateOf(t, consumers)
			if end.firstSeq != held[0] || end.lastSeq != held[1] {
				t.Fatalf("the stream holds %d to %d before the delete, want %d to %d, the two messages stored after the cut", end.firstSeq, end.lastSeq, held[0], held[1])
			}

			// The reconcile the restarted server made of the consumer kicked its state file's
			// write.
			waitForStoredState(t, owner, session)
			floor, err := consumers.Delete(ctx, session, nil)
			if err != nil {
				t.Fatalf("delete: %v", err)
			}

			requireFloorBeforeTheStreamsFirst(t, consumers, logs, session, id, floor, ackFloor(info), end)
			createdAtTheStreamsFirstSequence(t, again, owner, consumers, logs, session, id, floor, held)
		})
	}
}
