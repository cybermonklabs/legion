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

	"github.com/nats-io/nats.go/jetstream"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// clampedStartLine is the WARN line of a start past the stream's end, which create resets to the
// stream's first sequence. A production check reads it by this text, so the tests name it whole.
const clampedStartLine = "session consumer start past the stream's end, created at the stream's first sequence"

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

// streamStateOf is the stream's state, from a stream read that begins after the call
// (streamState): the state a create made next reads, when nothing is stored between the two.
func streamStateOf(t *testing.T, consumers *SessionConsumers) sessionStreamState {
	t.Helper()
	state, err := consumers.streamState(context.Background())
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	return state
}

// lastSeqOf is the stream's last sequence, from a stream read that begins after the call
// (streamStateOf): the end a create made next reads, when nothing is stored between the two.
func lastSeqOf(t *testing.T, consumers *SessionConsumers) uint64 {
	t.Helper()
	return streamStateOf(t, consumers).lastSeq
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

// requireNext requires pull to hand over the message stored at seq next, and fails the test with
// why that message is the one: a consumer that skipped it hands over nothing.
func requireNext(t *testing.T, pull *SessionPull, seq uint64, why string) SessionMessage {
	t.Helper()
	msg, err := pull.Next(testwait.Context(t))
	if err != nil {
		t.Fatalf("consumer %s handed over nothing (%v), want the message stored at %d: %s", pull.name, err, seq, why)
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
// the end that names the stream's identity; the pending start a restart (RestartAtFloor) left
// past the end; and the floor of a session's resume record plus one. Each is created at the
// stream's first sequence and answers that start; each writes one WARN line naming the start
// create was asked for (asked_start), the last sequence of the read that settled it
// (stream_last_seq), the stream's identity and the start it was created at, the stream's first
// sequence (consumer_start); and each is handed every message stored on the session's agent
// subject before the create, in stream order, and then the next one stored. A consumer created
// past the end skips all of them, and one created at the sequence after the stream's last skips
// the ones stored before it. A caller's start inside the stream, or at the sequence after its
// last, is created where it was asked and writes no line.
//
// The record is a source this build no longer writes: a delete whose floor is past the stream's
// end records the sequence before the stream's first (recordResumeFloorAt), so the floor plus one
// is the stream's first sequence and no create of this build reads a record past the end from one
// of its own deletes. The base build's delete recorded the floor its consumer held, past the end
// and all, so such a record stands through a rollout and after it for the stream's own retention,
// and the case below publishes one as that build's delete left it. A loss that cuts the stream
// below a record's floor removes the record, which the stream stored after it, too.
//
// The cases share the stream, whose first message is a bystander's at 1, so its first sequence is
// 1 in every case. Each stores its own messages and then reads the stream's last sequence just
// before its create, after the publishes of the cases before it and after its own source's, since
// the record's source stores a message of its own.
func TestASessionConsumerIsNeverCreatedPastTheStreamsEnd(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	for i := range 5 {
		publishStored(t, owner, contracts.LegacyAgentSubject("ses_past_end_bystander"), fmt.Sprintf("bystander %d", i))
	}
	id := streamIDOf(t, consumers)

	for _, tc := range []struct {
		name    string
		session string
		// source gives the case its start past the end, from the stream's last sequence before
		// it, and returns the start the case passes to Ensure and the start create is then asked
		// for, before the clamp. A source that stores a message of its own moves the stream's
		// end, which the case reads again after it.
		source func(t *testing.T, session string, lastSeq uint64) (startSeq, asked uint64)
	}{
		{
			name:    "a caller's start",
			session: "ses_past_end_caller",
			source: func(_ *testing.T, _ string, lastSeq uint64) (uint64, uint64) {
				// The start consumerFor builds from a resume of lastSeq + 2^20: the resume plus one.
				return lastSeq + pastTheEnd + 1, lastSeq + pastTheEnd + 1
			},
		},
		{
			name:    "a pending restart start",
			session: "ses_past_end_pending",
			source: func(_ *testing.T, session string, lastSeq uint64) (uint64, uint64) {
				// The start a restart leaves pending is the deleted consumer's floor plus one,
				// past the end for a consumer an earlier build created there; Ensure creates the
				// missing consumer at it, whatever its own start.
				name, _ := SessionConsumerName(session)
				consumers.setPendingStart(name, lastSeq+pastTheEnd)
				return 0, lastSeq + pastTheEnd
			},
		},
		{
			name:    "a resume record past the end",
			session: "ses_past_end_record",
			source: func(t *testing.T, session string, lastSeq uint64) (uint64, uint64) {
				// The record the base build's delete left for a consumer past the stream's end,
				// whose floor it recorded as the consumer held it: this build records the
				// sequence before the stream's first instead (recordResumeFloorAt), and such a
				// record stands through a rollout. A create with no start of its own starts at
				// its floor plus one, which is past the stream's end.
				record, err := json.Marshal(SessionResumeRecord{Floor: lastSeq + pastTheEnd, StreamID: id})
				if err != nil {
					t.Fatalf("marshal the resume record of %s: %v", session, err)
				}
				publishStored(t, owner, SessionResumeSubject(session), string(record))
				return 0, lastSeq + pastTheEnd + 1
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := contracts.LegacyAgentSubject(tc.session)
			// The messages stored for the session before its consumer is created: the stream's
			// last sequence, and so the sequence after it, is past each of them.
			var stored []uint64
			for i := range 3 {
				stored = append(stored, publishStored(t, owner, agent, fmt.Sprintf("stored before the create %d", i)))
			}
			state := streamStateOf(t, consumers)
			first, lastSeq := state.firstSeq, state.lastSeq
			if first != 1 {
				t.Fatalf("the stream's first sequence is %d, want 1, the first bystander's: the case needs a first sequence below the messages it stored at %v", first, stored)
			}
			startSeq, asked := tc.source(t, tc.session, lastSeq)
			// A source that stored a message of its own, as the resume record's did, moved the
			// stream's end, so the end the create reads is read again after it.
			lastSeq = lastSeqOf(t, consumers)
			start, err := consumers.Ensure(ctx, tc.session, []string{agent}, startSeq)
			if err != nil {
				t.Fatalf("Ensure: %v", err)
			}
			if want := (SessionConsumerStart{Created: true, StartSeq: first, StreamID: id, AckFloor: first - 1}); start != want {
				t.Errorf("Ensure asked for a start of %d, past the stream's last sequence %d = %+v; want %+v, the consumer created at the stream's first sequence", asked, lastSeq, start, want)
			}
			requireConsumerStart(t, consumers, tc.session, first, "the stream's first sequence")
			requireOneLine(t, logs, "WARN", clampedStartLine, tc.session, map[string]any{
				"asked_start":     asked,
				"stream_last_seq": lastSeq,
				"stream_id":       id,
				"consumer_start":  first,
			})

			written := publishStored(t, owner, agent, "written after the create")
			if written != lastSeq+1 {
				t.Fatalf("the message was stored at %d, want %d: something else was stored since the create read the stream", written, lastSeq+1)
			}
			pull := pullSession(t, consumers, tc.session)
			for _, seq := range stored {
				requireNext(t, pull, seq,
					"a message stored on the session's agent subject before the create; a consumer created past the stream's end, or at the sequence after its last, skips it")
			}
			requireNext(t, pull, written,
				"the first message stored after the create; a consumer created past the stream's end skips it")
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
			start, err := consumers.Ensure(ctx, tc.session, []string{contracts.LegacyAgentSubject(tc.session)}, want)
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
// retry that succeeds writes exactly one, so each line is one consumer created at the stream's
// first sequence.
func TestAFaultedCreateOfAClampedStartWritesNoLine(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	const session = "ses_past_end_faulted"
	filters := []string{contracts.LegacyAgentSubject(session)}
	for i := range 3 {
		publishStored(t, owner, contracts.LegacyAgentSubject("ses_past_end_faulted_bystander"), fmt.Sprintf("bystander %d", i))
	}
	state := streamStateOf(t, consumers)
	first, lastSeq := state.firstSeq, state.lastSeq
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
	if !start.Created || start.StartSeq != first {
		t.Errorf("Ensure after the fault was cleared = %+v; want the consumer created at %d, the stream's first sequence", start, first)
	}
	requireOneLine(t, logs, "WARN", clampedStartLine, session, map[string]any{
		"asked_start":     asked,
		"stream_last_seq": lastSeq,
		"stream_id":       streamIDOf(t, consumers),
		"consumer_start":  first,
	})
}

// A consumer an earlier build left past the stream's end is restarted at the stream's first
// sequence, on each path that would otherwise keep it: a restart at its floor (RestartAtFloor),
// which keeps a consumer with nothing awaiting an ack (keepAtFloor), and the relay's keep of a
// consumer as the consumer list read it (KeepListed), which answers not kept so the relay runs that
// restart (cmd/listener session_relay.go). nats-server v2.15.1 reports such a consumer's ack floor
// as the sequence before its start, OptStartSeq - 1, above the stream's last sequence, so its next
// sequence, its old start, is past the stream's end. So the restart deletes the consumer and
// creates it again at that old start, which create clamps to the stream's first sequence, writing
// the clamp's one line, and the consumer is handed every message stored on the session's agent
// subject below its old start, which the consumer as it was skips, and then the next one stored.
//
// The consumer is created as that build created it, with the settings and filters this build gives
// the subjects each path is passed (createAsAnEarlierBuild), so only its start can make a path
// restart it; a consumer created the same way at the sequence after the stream's last is kept.
func TestAConsumerAnEarlierBuildLeftPastTheEndIsRestartedAtTheStreamsFirstSequence(t *testing.T) {
	owner, consumers, logs := sessionConsumersLogging(t)
	ctx := context.Background()
	for i := range 3 {
		publishStored(t, owner, contracts.LegacyAgentSubject("ses_kept_past_end_bystander"), fmt.Sprintf("bystander %d", i))
	}
	id := streamIDOf(t, consumers)
	state, err := consumers.streamState(ctx)
	if err != nil {
		t.Fatalf("read the stream's state: %v", err)
	}
	// first is the stream's first sequence, which nothing in the test moves: the stream ages out
	// no message within it.
	first := state.firstSeq

	// below stores two messages on session's agent subject after its consumer was created at
	// lastSeq + 2^20, the stream's last sequence then being lastSeq, and returns their sequences,
	// lastSeq + 1 and lastSeq + 2: both below the consumer's start.
	below := func(t *testing.T, session string, lastSeq uint64) []uint64 {
		t.Helper()
		var held []uint64
		for i := range 2 {
			seq := publishStored(t, owner, contracts.LegacyAgentSubject(session), fmt.Sprintf("below the old start %d", i))
			if want := lastSeq + uint64(i) + 1; seq != want {
				t.Fatalf("message %d on the agent subject of %s was stored at %d, want %d: something else was stored since the stream was read", i, session, seq, want)
			}
			held = append(held, seq)
		}
		return held
	}
	// restarted requires session's consumer, created at lastSeq + 2^20 and restarted with the
	// stream's last sequence at lastSeq + 2, to start at the stream's first sequence, and the
	// clamp's one line for it: the restart asked create for lastSeq + 2^20, the sequence after
	// the floor the server reports, lastSeq + 2^20 - 1.
	restarted := func(t *testing.T, session string, lastSeq uint64, start SessionConsumerStart) {
		t.Helper()
		if want := (SessionConsumerStart{Created: true, StartSeq: first, StreamID: id, AckFloor: first - 1}); start != want {
			t.Errorf("RestartAtFloor of a consumer created at %d, past the stream's last sequence %d = %+v; want %+v, the consumer created again at the stream's first sequence", lastSeq+pastTheEnd, lastSeq+2, start, want)
		}
		requireConsumerStart(t, consumers, session, first, "the stream's first sequence, where the restart created it again")
		requireOneLine(t, logs, "WARN", clampedStartLine, session, map[string]any{
			"asked_start":     lastSeq + pastTheEnd,
			"stream_last_seq": lastSeq + 2,
			"stream_id":       id,
			"consumer_start":  first,
		})
	}
	// delivered requires session's consumer, whichever path it took, to be handed the messages
	// stored below its old start, held, and then the next message stored on its agent subject, in
	// stream order.
	delivered := func(t *testing.T, session string, held []uint64) {
		t.Helper()
		written := publishStored(t, owner, contracts.LegacyAgentSubject(session), "written after the path ran")
		if want := held[len(held)-1] + 1; written != want {
			t.Fatalf("the message was stored at %d, want %d: something else was stored since the messages below the old start", written, want)
		}
		pull := pullSession(t, consumers, session)
		for _, seq := range held {
			requireNext(t, pull, seq, "a message stored below the old start; a consumer left past the stream's end, or restarted at the stream's end, skips it")
		}
		requireNext(t, pull, written, "the next message stored on the session's agent subject")
	}

	t.Run("RestartAtFloor", func(t *testing.T) {
		const session = "ses_kept_past_end_restarted"
		filters := []string{contracts.LegacyAgentSubject(session)}
		lastSeq := createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)
		held := below(t, session, lastSeq)
		start, err := consumers.RestartAtFloor(ctx, session, filters)
		if err != nil {
			t.Fatalf("restart at floor: %v", err)
		}
		restarted(t, session, lastSeq, start)
		delivered(t, session, held)
	})

	t.Run("KeepListed", func(t *testing.T) {
		const session, atTheEnd = "ses_kept_past_end_listed", "ses_kept_at_end_listed"
		filters, atTheEndFilters := []string{contracts.LegacyAgentSubject(session)}, []string{contracts.LegacyAgentSubject(atTheEnd)}
		createAsAnEarlierBuild(t, consumers, atTheEnd, atTheEndFilters, 1)
		lastSeq := createAsAnEarlierBuild(t, consumers, session, filters, pastTheEnd)
		held := below(t, session, lastSeq)
		listed, err := consumers.ListSessionConsumers(ctx)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if listed[session] == nil || listed[atTheEnd] == nil {
			t.Fatalf("the list holds %v for %s and %v for %s, want both consumers", listed[session], session, listed[atTheEnd], atTheEnd)
		}
		if _, kept, err := consumers.KeepListed(ctx, atTheEnd, atTheEndFilters, listed[atTheEnd]); err != nil || !kept {
			t.Fatalf("KeepListed of a consumer created the same way at %d, the sequence after the stream's last when it was created = %t, %v; want it kept, since it differs from the one past the end only in its start", lastSeq+1, kept, err)
		}

		// The relay pulls a consumer KeepListed kept, and restarts one it did not.
		start, kept, err := consumers.KeepListed(ctx, session, filters, listed[session])
		if err != nil {
			t.Fatalf("KeepListed: %v", err)
		}
		if kept {
			t.Errorf("KeepListed of a consumer created at %d, past the stream's last sequence %d = %+v, kept; want it not kept, so the relay restarts it", lastSeq+pastTheEnd, lastSeq+2, start)
		} else {
			again, err := consumers.RestartAtFloor(ctx, session, filters)
			if err != nil {
				t.Fatalf("restart at floor: %v", err)
			}
			restarted(t, session, lastSeq, again)
		}
		delivered(t, session, held)
	})
}

// pastStreamEnd judges a sequence past the end of the read it was handed against a second stream
// read, and answers past the end only where the sequence is past that newer read's end too, so a
// sequence that is not past the stream's real end is never read as past it. The read it hands
// back is the one that settled it, which the caller takes its Gap, its StreamID and, for a start
// it resets, its first sequence from (create, keepAtFloor).
//
// The read a caller holds can trail the sequence it fixed: create's first read can begin before
// the message its start is then fixed to was stored (Client.NextStoredMessage), and keepAtFloor's
// read is joined before the consumer's info is read (RestartAtFloor), so it can begin before the
// consumer's own next sequence was reached. This case is that read: the stream's real state with
// its last sequence lowered by two, as a read that began two messages ago holds it. Each sequence
// above that stale end and at or below the sequence after the stream's real last is not past the
// end, and the read that says so is the real one. A sequence past the stream's real end is the
// control: it is past the end, from that same second read.
func TestASequencePastAStaleStreamReadIsJudgedAgainstASecondRead(t *testing.T) {
	owner, consumers := sessionConsumersOnStream(t)
	ctx := context.Background()
	for i := range 3 {
		publishStored(t, owner, contracts.LegacyAgentSubject("ses_past_stream_end"), fmt.Sprintf("stored %d", i))
	}
	live := streamStateOf(t, consumers)
	if live.lastSeq < 3 {
		t.Fatalf("the stream's last sequence is %d, want at least 3, the messages stored above: the case needs a read it can lower", live.lastSeq)
	}
	stale := live
	stale.lastSeq = live.lastSeq - 2

	for _, seq := range []uint64{live.lastSeq, live.lastSeq + 1} {
		read, past, err := consumers.pastStreamEnd(ctx, stale, seq)
		if err != nil {
			t.Fatalf("pastStreamEnd of %d against a read that ends at %d: %v", seq, stale.lastSeq, err)
		}
		if past {
			t.Errorf("pastStreamEnd of %d against a read that ends at %d = past the end; want it judged against the stream's own end %d, which it is not past", seq, stale.lastSeq, live.lastSeq)
		}
		if read != live {
			t.Errorf("pastStreamEnd of %d against a read that ends at %d answered the read %+v; want %+v, the second read it made, whose last sequence is the stream's own", seq, stale.lastSeq, read, live)
		}
	}

	beyond := live.lastSeq + 2
	read, past, err := consumers.pastStreamEnd(ctx, stale, beyond)
	if err != nil {
		t.Fatalf("pastStreamEnd of %d, past the stream's end: %v", beyond, err)
	}
	if !past {
		t.Errorf("pastStreamEnd of %d = not past the end, the stream's last sequence being %d; want it past the end", beyond, live.lastSeq)
	}
	if read != live {
		t.Errorf("pastStreamEnd of %d answered the read %+v; want %+v, the second read it made", beyond, read, live)
	}
}
