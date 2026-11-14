package bus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
)

// The WARN lines of a start and of a floor past the stream's end (create, recordResumeFloorAt). A
// production check reads them by these texts, so the tests name them whole.
const (
	clampedStartLine = "session consumer start past the stream's end, created at the sequence after the stream's last"
	cappedFloorLine  = "session resume floor past the stream's end, recorded at the stream's last sequence"
)

// pastTheEnd is how far past the stream's last sequence a case puts its start: 2^20, far past any
// sequence a test's stream reaches, so the start is past the end however many messages the case
// stored first.
const pastTheEnd = 1 << 20

// sessionConsumersLogging is sessionConsumersOnStream with a logger of the test's own: the
// SessionConsumers writes its lines (create, recordResumeFloorAt) as JSON into the returned logs,
// as it writes them through the listener's JSON logger (ConnectSessionConsumers), and the
// process's default logger is left alone.
func sessionConsumersLogging(t *testing.T) (*Client, *SessionConsumers, *capturedLogs) {
	t.Helper()
	uri := testnats.URL(t)
	owner, err := ConnectOwningStream([]string{uri})
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
	return owner, consumers, logs
}

// sessionLines is every line logs holds whose msg is message and whose session_id is sessionID,
// decoded with its numbers as json.Number, so a sequence reads as the digits slog.Uint64 wrote.
func sessionLines(t *testing.T, logs *capturedLogs, message, sessionID string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(logs.String()) {
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode log line %q: %v", line, err)
		}
		if record["msg"] == message && record["session_id"] == sessionID {
			lines = append(lines, record)
		}
	}
	return lines
}

// requireOneLine requires logs to hold exactly one line of message for sessionID, at level, whose
// keys hold want's values: a uint64 as the number slog.Uint64 logged, a string as it is. It fails
// the test without stopping it, so the case goes on to show what the consumer delivers.
func requireOneLine(t *testing.T, logs *capturedLogs, level, message, sessionID string, want map[string]any) {
	t.Helper()
	lines := sessionLines(t, logs, message, sessionID)
	if len(lines) != 1 {
		t.Errorf("the log holds %d lines %q for session %s, want exactly one: %v", len(lines), message, sessionID, lines)
		return
	}
	line := lines[0]
	if line["level"] != level {
		t.Errorf("the line %q for session %s is at level %v, want %s", message, sessionID, line["level"], level)
	}
	for _, key := range slices.Sorted(maps.Keys(want)) {
		value := want[key]
		if number, ok := value.(uint64); ok {
			value = json.Number(strconv.FormatUint(number, 10))
		}
		if line[key] != value {
			t.Errorf("the line %q for session %s has %s = %v, want %v", message, sessionID, key, line[key], value)
		}
	}
}

// lastSeqOf is the stream's last sequence, from a stream read that begins after the call
// (streamState): the end a create made next reads, when nothing is stored between the two.
func lastSeqOf(t *testing.T, consumers *SessionConsumers) uint64 {
	t.Helper()
	state, err := consumers.streamState(context.Background())
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	return state.lastSeq
}

// requireConsumerStart requires sessionID's consumer, as the server reports it, to start at stream
// sequence want (its OptStartSeq), which why says the reason for. It fails the test without
// stopping it.
func requireConsumerStart(t *testing.T, consumers *SessionConsumers, sessionID string, want uint64, why string) {
	t.Helper()
	info, err := consumers.Info(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("info of %s: %v", sessionID, err)
	}
	if info.Config.OptStartSeq != want {
		t.Errorf("the server reports the consumer of %s starting at %d, want %d: %s", sessionID, info.Config.OptStartSeq, want, why)
	}
}

// requireNext requires pull to hand over the message stored at seq next, within 5 s, and fails the
// test with why that message is the one: a consumer that skipped it hands over nothing.
func requireNext(t *testing.T, pull *SessionPull, seq uint64, why string) SessionMessage {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msg, err := pull.Next(ctx)
	if err != nil {
		t.Fatalf("consumer %s handed over nothing within 5s (%v), want the message stored at %d: %s", pull.name, err, seq, why)
	}
	if msg.StreamSeq != seq {
		t.Fatalf("consumer %s handed over the message stored at %d, want the one at %d: %s", pull.name, msg.StreamSeq, seq, why)
	}
	return msg
}

// createAsAnEarlierBuild creates sessionID's consumer at the stream's last sequence plus past, as a
// build from before create clamped every start created one for a start past the stream's end, and
// returns that last sequence, read just before the create. The consumer carries the session
// consumer settings and the filters subjects reduce to (applySettings, sessionConsumerTarget), as
// a create of this build does, so a caller passing the same subjects finds nothing of it to change
// but its start (updated).
func createAsAnEarlierBuild(t *testing.T, consumers *SessionConsumers, sessionID string, subjects []string, past uint64) uint64 {
	t.Helper()
	ctx := context.Background()
	name, filters, err := sessionConsumerTarget(sessionID, subjects)
	if err != nil {
		t.Fatalf("name the consumer of %s: %v", sessionID, err)
	}
	lastSeq := lastSeqOf(t, consumers)
	config := jetstream.ConsumerConfig{
		Durable:       name,
		DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq:   lastSeq + past,
	}
	consumers.applySettings(&config, filters)
	if _, err := consumers.upsert(ctx, name, config, "create"); err != nil {
		t.Fatalf("create the consumer of %s at %d: %v", sessionID, lastSeq+past, err)
	}
	info, err := consumers.Info(ctx, sessionID)
	if err != nil {
		t.Fatalf("info of %s: %v", sessionID, err)
	}
	t.Logf("consumer %s created at %d, the stream's last sequence being %d: the server reports its ack floor as %d and its last delivery as %d, and the floor this package reads from that is %d",
		name, info.Config.OptStartSeq, lastSeq, info.AckFloor.Stream, info.Delivered.Stream, ackFloor(info))
	return lastSeq
}

// A session consumer is never created past the stream's end, whichever source its start came from
// (create): a caller's start, which consumerFor (cmd/listener) builds from a client's resume past
// the end that names the stream's identity; the floor of a resume record past the end, which a
// delete of a consumer an earlier build left there recorded before recordResumeFloorAt capped a
// floor at the stream's last sequence; and the pending start a restart (RestartAtFloor) left past
// the end. Each is created at the sequence after the stream's last and answers that start; each
// writes one WARN line naming the start create was asked for (asked_start), the last sequence of
// the read it was clamped to (stream_last_seq), the stream's identity and the start it was created
// at (consumer_start); and each is handed the next message stored on the session's agent subject,
// which a consumer past the end skips. A caller's start inside the stream, or at the sequence after
// its last, is created where it was asked and writes no line.
//
// The cases share the stream, so each reads the stream's last sequence just before its create,
// after the publishes of the cases before it and its own record's.
func TestASessionConsumerIsNeverCreatedPastTheStreamsEnd(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	for i := range 5 {
		publishStored(t, owner, contracts.AgentSubject("ses_past_end_bystander"), fmt.Sprintf("bystander %d", i))
	}
	id := streamIDOf(t, consumers)

	for _, tc := range []struct {
		name    string
		session string
		// source gives the case its start past the end, from the stream's last sequence before
		// it, and returns the start the case passes to Ensure and the start create is then asked
		// for, before the clamp.
		source func(t *testing.T, session string, lastSeq uint64) (startSeq, asked uint64)
	}{
		{
			name:    "a caller's start",
			session: "ses_past_end_caller",
			source: func(t *testing.T, _ string, lastSeq uint64) (uint64, uint64) {
				// The start consumerFor builds from a resume of lastSeq + 2^20: the resume plus one.
				return lastSeq + pastTheEnd + 1, lastSeq + pastTheEnd + 1
			},
		},
		{
			name:    "a resume record's floor",
			session: "ses_past_end_record",
			source: func(t *testing.T, session string, lastSeq uint64) (uint64, uint64) {
				// Published raw, as a delete wrote a floor past the end before recordResumeFloorAt
				// capped it; it names this stream, so a create with no start of its own trusts it.
				record, err := json.Marshal(SessionResumeRecord{Floor: lastSeq + pastTheEnd, StreamID: id})
				if err != nil {
					t.Fatalf("marshal the resume record: %v", err)
				}
				publishStored(t, owner, SessionResumeSubject(session), string(record))
				return 0, lastSeq + pastTheEnd + 1
			},
		},
		{
			name:    "a pending restart start",
			session: "ses_past_end_pending",
			source: func(t *testing.T, session string, lastSeq uint64) (uint64, uint64) {
				// The start a restart leaves pending is the deleted consumer's floor plus one,
				// past the end for a consumer an earlier build created there; Ensure creates the
				// missing consumer at it, whatever its own start.
				name, _ := SessionConsumerName(session)
				consumers.setPendingStart(name, lastSeq+pastTheEnd)
				return 0, lastSeq + pastTheEnd
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := contracts.AgentSubject(tc.session)
			startSeq, asked := tc.source(t, tc.session, lastSeqOf(t, consumers))
			lastSeq := lastSeqOf(t, consumers)
			start, err := consumers.Ensure(ctx, tc.session, []string{agent}, startSeq)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if want := (SessionConsumerStart{Created: true, StartSeq: lastSeq + 1, StreamID: id, AckFloor: lastSeq}); start != want {
				t.Errorf("Ensure asked for a start of %d, past the stream's last sequence %d = %+v; want %+v, the consumer created at the sequence after the stream's last", asked, lastSeq, start, want)
			}
			requireConsumerStart(t, consumers, tc.session, lastSeq+1, "the sequence after the stream's last")
			requireOneLine(t, logs, "WARN", clampedStartLine, tc.session, map[string]any{
				"asked_start":     asked,
				"stream_last_seq": lastSeq,
				"stream_id":       id,
				"consumer_start":  lastSeq + 1,
			})

			written := publishStored(t, owner, agent, "written after the create")
			if written != lastSeq+1 {
				t.Fatalf("the message was stored at %d, want %d: something else was stored since the create read the stream", written, lastSeq+1)
			}
			requireNext(t, pullSession(t, consumers, tc.session), written,
				"the first message stored after the create, at the consumer's start; a consumer created past the stream's end skips it")
		})
	}

	for _, tc := range []struct {
		name    string
		session string
		// start is the caller's start, from the stream's last sequence.
		start func(lastSeq uint64) uint64
	}{
		{"a caller's start inside the stream", "ses_past_end_inside", func(lastSeq uint64) uint64 { return lastSeq }},
		{"a caller's start at the sequence after the stream's last", "ses_past_end_at_end", func(lastSeq uint64) uint64 { return lastSeq + 1 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lastSeq := lastSeqOf(t, consumers)
			want := tc.start(lastSeq)
			start, err := consumers.Ensure(ctx, tc.session, []string{contracts.AgentSubject(tc.session)}, want)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if !start.Created || start.StartSeq != want {
				t.Errorf("Ensure asked for a start of %d, the stream's last sequence being %d = %+v; want the consumer created there, which is not past the stream's end", want, lastSeq, start)
			}
			requireConsumerStart(t, consumers, tc.session, want, "the start asked, which is not past the stream's end")
			if lines := sessionLines(t, logs, clampedStartLine, tc.session); len(lines) != 0 {
				t.Errorf("the log holds %v for session %s, whose start %d is not past the stream's last sequence %d; want no line", lines, tc.session, want, lastSeq)
			}
		})
	}
}

// A clamp's line is written once the server confirmed the create, and only then (create): a create
// of a start past the end that faulted (SetCreateFault) writes none and leaves no consumer, and the
// retry that succeeds writes exactly one, so each line is one consumer created at the stream's end.
func TestAFaultedCreateOfAClampedStartWritesNoLine(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_past_end_faulted"
	filters := []string{contracts.AgentSubject(session)}
	for i := range 3 {
		publishStored(t, owner, contracts.AgentSubject("ses_past_end_faulted_bystander"), fmt.Sprintf("bystander %d", i))
	}
	lastSeq := lastSeqOf(t, consumers)
	asked := lastSeq + pastTheEnd + 1

	consumers.SetCreateFault(func(string) error { return errors.New("the create faulted") })
	_, err := consumers.Ensure(ctx, session, filters, asked)
	consumers.SetCreateFault(nil)
	if err == nil {
		t.Fatal("Ensure whose create faulted reported no error")
	}
	if lines := sessionLines(t, logs, clampedStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for a create that faulted; want no line: the server created no consumer", lines)
	}
	if _, err := consumers.Info(ctx, session); !errors.Is(err, ErrSessionConsumerNotFound) {
		t.Fatalf("the consumer's info after the faulted create = %v, want ErrSessionConsumerNotFound", err)
	}

	start, err := consumers.Ensure(ctx, session, filters, asked)
	if err != nil {
		t.Fatalf("Ensure after the fault was cleared: %v", err)
	}
	if !start.Created || start.StartSeq != lastSeq+1 {
		t.Errorf("Ensure after the fault was cleared = %+v; want the consumer created at %d, the sequence after the stream's last", start, lastSeq+1)
	}
	requireOneLine(t, logs, "WARN", clampedStartLine, session, map[string]any{
		"asked_start":     asked,
		"stream_last_seq": lastSeq,
		"stream_id":       streamIDOf(t, consumers),
		"consumer_start":  lastSeq + 1,
	})
}

// A consumer an earlier build left past the stream's end is restarted at the sequence after the
// stream's last, on each path that would otherwise keep it: a restart at its floor
// (RestartAtFloor), which keeps a consumer with nothing awaiting an ack (keepAtFloor), and the
// relay's keep of a consumer as the consumer list read it (KeepListed), which answers not kept so
// the relay runs that restart (cmd/listener session_relay.go). The restart's create clamps the
// consumer's next sequence, writes the clamp's line, and the consumer is handed the next message
// stored on the session's agent subject, which the consumer as it was would skip.
//
// The consumer is created as that build created it, with the settings and filters this build gives
// the subjects each path is passed (createAsAnEarlierBuild), so only its start can make a path
// restart it; a consumer created the same way at the sequence after the stream's last is kept.
func TestAConsumerAnEarlierBuildLeftPastTheEndIsRestartedAtTheStreamsEnd(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	for i := range 3 {
		publishStored(t, owner, contracts.AgentSubject("ses_kept_past_end_bystander"), fmt.Sprintf("bystander %d", i))
	}
	id := streamIDOf(t, consumers)

	// restarted requires session's consumer, restarted with the stream's last sequence at
	// lastSeq, to start at the sequence after it, and the clamp's one line for it: the restart's
	// start is the consumer's next sequence, lastSeq + 2^20 (ackFloor reads the sequence before
	// the start of a consumer that has acked nothing).
	restarted := func(t *testing.T, session string, lastSeq uint64, start SessionConsumerStart) {
		t.Helper()
		if want := (SessionConsumerStart{Created: true, StartSeq: lastSeq + 1, StreamID: id, AckFloor: lastSeq}); start != want {
			t.Errorf("RestartAtFloor of a consumer created at %d, past the stream's last sequence %d = %+v; want %+v, the consumer created again at the sequence after the stream's last", lastSeq+pastTheEnd, lastSeq, start, want)
		}
		requireConsumerStart(t, consumers, session, lastSeq+1, "the sequence after the stream's last, where the restart created it again")
		requireOneLine(t, logs, "WARN", clampedStartLine, session, map[string]any{
			"asked_start":     lastSeq + pastTheEnd,
			"stream_last_seq": lastSeq,
			"stream_id":       id,
			"consumer_start":  lastSeq + 1,
		})
	}
	// delivered requires session's consumer, whichever path it took, to be handed the next message
	// stored on its agent subject, at lastSeq + 1.
	delivered := func(t *testing.T, session string, lastSeq uint64) {
		t.Helper()
		written := publishStored(t, owner, contracts.AgentSubject(session), "written after the path ran")
		if written != lastSeq+1 {
			t.Fatalf("the message was stored at %d, want %d: something else was stored since the stream was read", written, lastSeq+1)
		}
		requireNext(t, pullSession(t, consumers, session), written,
			"the first message stored after the stream's last sequence; a consumer left past the stream's end skips it")
	}

	t.Run("RestartAtFloor", func(t *testing.T) {
		const session = "ses_kept_past_end_restarted"
		filters := []string{contracts.AgentSubject(session)}
		lastSeq := createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)
		start, err := consumers.RestartAtFloor(ctx, session, filters)
		if err != nil {
			t.Fatalf("restart at floor: %v", err)
		}
		restarted(t, session, lastSeq, start)
		delivered(t, session, lastSeq)
	})

	t.Run("KeepListed", func(t *testing.T) {
		const session, atTheEnd = "ses_kept_past_end_listed", "ses_kept_at_end_listed"
		filters, atTheEndFilters := []string{contracts.AgentSubject(session)}, []string{contracts.AgentSubject(atTheEnd)}
		createAsAnEarlierBuild(t, consumers, atTheEnd, atTheEndFilters, 1)
		lastSeq := createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)
		listed, err := consumers.ListSessionConsumers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if listed[session] == nil || listed[atTheEnd] == nil {
			t.Fatalf("the list holds %v for %s and %v for %s, want both consumers", listed[session], session, listed[atTheEnd], atTheEnd)
		}
		if _, kept, err := consumers.KeepListed(ctx, atTheEnd, atTheEndFilters, listed[atTheEnd]); err != nil || !kept {
			t.Fatalf("KeepListed of a consumer created the same way at %d, the sequence after the stream's last = %t, %v; want it kept, since it differs from the one past the end only in its start", lastSeq+1, kept, err)
		}

		// The relay pulls a consumer KeepListed kept, and restarts one it did not.
		start, kept, err := consumers.KeepListed(ctx, session, filters, listed[session])
		if err != nil {
			t.Fatalf("KeepListed: %v", err)
		}
		if kept {
			t.Errorf("KeepListed of a consumer created at %d, past the stream's last sequence %d = %+v, kept; want it not kept, so the relay restarts it", lastSeq+pastTheEnd, lastSeq, start)
		} else {
			again, err := consumers.RestartAtFloor(ctx, session, filters)
			if err != nil {
				t.Fatalf("restart at floor: %v", err)
			}
			restarted(t, session, lastSeq, again)
		}
		delivered(t, session, lastSeq)
	})
}

// A delete of a consumer an earlier build left past the stream's end records the stream's last
// sequence as the session's floor and answers it (Delete, recordResumeFloorAt): the consumer's
// floor, the sequence before its start, names no stored message, and a record of it would start
// the session's next consumer past every message stored until the stream grows past that floor.
// The cap writes one WARN line naming the floor asked (floor), the stream's last sequence
// (stream_last_seq) and the stream's identity, and the consumer created again from the record
// (Ensure with no start of its own) starts at the sequence after that last one, is no clamp, and
// writes no clamp line. The record's own publish moves the stream's end by one, so the case
// compares with the last sequence read before the delete.
func TestADeleteOfAConsumerPastTheEndRecordsTheStreamsLastSequence(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_deleted_past_end"
	agent := contracts.AgentSubject(session)
	filters := []string{agent}
	for i := range 3 {
		publishStored(t, owner, contracts.AgentSubject("ses_deleted_past_end_bystander"), fmt.Sprintf("bystander %d", i))
	}
	id := streamIDOf(t, consumers)
	createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)
	info, err := consumers.Info(ctx, session)
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	lastSeq := lastSeqOf(t, consumers)

	floor, err := consumers.Delete(ctx, session, nil)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if floor != lastSeq {
		t.Errorf("the delete of a consumer created at %d answered the floor %d, want %d, the stream's last sequence", info.Config.OptStartSeq, floor, lastSeq)
	}
	stored, err := consumers.client.LastStoredMessage(ctx, SessionResumeSubject(session))
	if err != nil {
		t.Fatalf("read the session's resume record: %v", err)
	}
	var record SessionResumeRecord
	if err := json.Unmarshal(stored.Data, &record); err != nil {
		t.Fatalf("decode the session's resume record %q: %v", stored.Data, err)
	}
	if want := (SessionResumeRecord{Floor: lastSeq, StreamID: id}); record != want {
		t.Errorf("the delete recorded %+v, want %+v: the stream's last sequence, not the floor %d past it", record, want, info.Config.OptStartSeq-1)
	}
	requireOneLine(t, logs, "WARN", cappedFloorLine, session, map[string]any{
		"floor":           info.Config.OptStartSeq - 1,
		"stream_last_seq": lastSeq,
		"stream_id":       id,
	})

	start, err := consumers.Ensure(ctx, session, filters, 0)
	if err != nil {
		t.Fatalf("ensure after the delete: %v", err)
	}
	if !start.Created || start.StartSeq != lastSeq+1 {
		t.Errorf("Ensure after the delete = %+v; want the consumer created at %d, the sequence after the floor the delete recorded", start, lastSeq+1)
	}
	requireConsumerStart(t, consumers, session, lastSeq+1, "the sequence after the floor the delete recorded")
	if lines := sessionLines(t, logs, clampedStartLine, session); len(lines) != 0 {
		t.Errorf("the log holds %v for the create from the record; want no clamp line: the recorded floor is not past the stream's end", lines)
	}
	written := publishStored(t, owner, agent, "written after the create")
	requireNext(t, pullSession(t, consumers, session), written,
		"the first message stored on the session's agent subject after the floor the delete recorded")
}
