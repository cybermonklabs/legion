package main

import (
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
)

// The lines a session consumer's create writes of a start it moved (bus.SessionConsumers.create):
// the WARN of a start past the stream's end it created at the sequence after the stream's last,
// and the INFO of a caller's start it lowered to the session's resume record. A production check
// reads them by these texts, so the tests name them whole.
const (
	clampedStartLine = "session consumer start past the stream's end, created at the sequence after the stream's last"
	loweredStartLine = "session consumer created at its resume record's floor, below the start the caller asked"
)

// pastTheEnd is how far past the stream's last sequence a registration puts its resume point: 2^20,
// far past any sequence these tests' streams reach, as the fixture of a session that came back
// from a restored or recreated stream with the point it held there.
const pastTheEnd = 1 << 20

// streamLastSeq is the last sequence the stream holds, as the server reports it.
func (rig *sessionConsumerRig) streamLastSeq(t *testing.T) uint64 {
	t.Helper()
	info, err := rig.harness.client.JS().StreamInfo(bus.Stream)
	if err != nil {
		t.Fatalf("read the stream's info: %v", err)
	}
	return info.State.LastSeq
}

// logLines is every line of the listener's log whose msg is message and whose session_id is
// sessionID, decoded with its numbers as json.Number, so a sequence reads as the digits
// slog.Uint64 wrote. Every line is the JSON of the listener's own logger, so one that does not
// decode fails the test.
func (rig *sessionConsumerRig) logLines(t *testing.T, message, sessionID string) []map[string]any {
	t.Helper()
	var lines []map[string]any
	for line := range strings.Lines(rig.harness.logs.String()) {
		decoder := json.NewDecoder(strings.NewReader(line))
		decoder.UseNumber()
		var record map[string]any
		if err := decoder.Decode(&record); err != nil {
			t.Fatalf("decode the listener's log line %q: %v", line, err)
		}
		if record["msg"] == message && record["session_id"] == sessionID {
			lines = append(lines, record)
		}
	}
	return lines
}

// requireOneLogLine requires the listener's log to hold exactly one line of message for sessionID,
// at level, carrying a machine_id and want's values: a uint64 as the number slog.Uint64 logged, a
// string as it is. The machine_id is what says the line came through the listener's own JSON
// logger (logging.Logger.Slog, which connectSessionDelivery hands the bus) rather than the
// process's default. It fails the test without stopping it, so the case goes on to show what the
// consumer delivers.
func (rig *sessionConsumerRig) requireOneLogLine(t *testing.T, level, message, sessionID string, want map[string]any) {
	t.Helper()
	lines := rig.logLines(t, message, sessionID)
	if len(lines) != 1 {
		t.Errorf("the listener logged %d lines %q for session %s, want exactly one: %v", len(lines), message, sessionID, lines)
		return
	}
	line := lines[0]
	if line["level"] != level {
		t.Errorf("the line %q for session %s is at level %v, want %s", message, sessionID, line["level"], level)
	}
	if machineID, _ := line["machine_id"].(string); machineID == "" {
		t.Errorf("the line %q for session %s carries no machine_id, so it is not the listener's own JSON logger's: %v", message, sessionID, line)
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

// A registration whose resume point is past the stream's end — a point a session brings back from
// a stream that was restored or recreated under it, or one an earlier build's consumer created past
// the end handed it — is answered, and its consumer created, at the sequence after the stream's
// last, so the consumer delivers what the stream stores from then on rather than skipping every
// message until the stream grows past that point (bus.SessionConsumers.create). The listener says
// so in one WARN line of its own JSON log, with its machine_id, naming the session, the start the
// registration asked for (its resume point plus one), the stream's last sequence and identity, and
// the start the consumer was given. A registration whose resume point is inside the stream is
// answered at the sequence after that point and writes no such line, so the line is the clamp's
// alone.
func TestARegistrationWhoseResumePointIsPastTheStreamsEndIsCreatedAtTheEndAndLogged(t *testing.T) {
	rig := newSessionConsumerRig(t)
	const sessionID, inside = "ses_resume_past_end", "ses_resume_inside_the_stream"
	// Another session's messages, so the stream's last sequence is past its first and a resume
	// point inside the stream names a message of it.
	for range 5 {
		rig.store(t, contracts.AgentSubject("ses_resume_past_end_bystander"))
	}
	lastSeq := rig.streamLastSeq(t)
	streamID := rig.streamID(t)

	answer := rig.subscribe(t, consumerResumeRegistration(sessionID, "e", streamID, lastSeq+pastTheEnd))

	if answer.Delivery == nil || answer.Delivery.Mode != "consumer" {
		t.Fatalf("answer %s, want delivery mode consumer", answer.raw)
	}
	if answer.Delivery.ConsumerStart != lastSeq+1 {
		t.Errorf("the answer carries consumer_start %d, want %d, the sequence after the stream's last %d, not the %d the resume point asked",
			answer.Delivery.ConsumerStart, lastSeq+1, lastSeq, lastSeq+pastTheEnd+1)
	}
	if start := rig.startSeq(t, sessionID); start != lastSeq+1 {
		t.Errorf("the consumer of %s starts at %d, want %d, the sequence after the stream's last %d", sessionID, start, lastSeq+1, lastSeq)
	}
	rig.requireOneLogLine(t, "WARN", clampedStartLine, sessionID, map[string]any{
		"asked_start":     lastSeq + pastTheEnd + 1,
		"stream_last_seq": lastSeq,
		"stream_id":       streamID,
		"consumer_start":  lastSeq + 1,
	})

	// A session with no resume record, whose resume point names a message the stream holds.
	resume := lastSeq - 2
	answer = rig.subscribe(t, consumerResumeRegistration(inside, "e", streamID, resume))

	if answer.Delivery == nil || answer.Delivery.ConsumerStart != resume+1 {
		t.Errorf("answer %s, want consumer_start %d, the sequence after its resume point %d inside the stream", answer.raw, resume+1, resume)
	}
	if start := rig.startSeq(t, inside); start != resume+1 {
		t.Errorf("the consumer of %s starts at %d, want %d, the sequence after its resume point", inside, start, resume+1)
	}
	if lines := rig.logLines(t, clampedStartLine, inside); len(lines) != 0 {
		t.Errorf("the listener logged %d lines %q for %s, whose resume point is inside the stream, want none: %v", len(lines), clampedStartLine, inside, lines)
	}
}

// A session whose consumer was once created at the stream's end, because its resume point was past
// it, comes back after a deregistration with that same resume point — a client's resume point only
// rises, and nothing it wrote since was above it — and loses nothing stored while it was away. The
// delete of its consumer recorded the ack floor F of the messages it took and acked (the session's
// resume record, bus.SessionResumeSubject), and the registration lowers the start its resume point
// asks to F + 1, where without the record that start would be clamped to the stream's end again,
// past every message stored while the session was away (bus.SessionConsumers.create). So its next
// consumer starts at F + 1, the answer says so, and the session is handed the messages stored while
// it was away and nothing it had acked. The listener logs the lowering in one INFO line naming the
// session, the start the registration asked for, the record's floor and the stream's identity, and
// the second create, which the record kept inside the stream, adds no clamp line to the first's.
func TestASessionClampedOnceComesBackFromItsResumeRecordAndLosesNothing(t *testing.T) {
	rig := newRelayRig(t)
	const sessionID = "ses_resume_clamped_once"
	agent := contracts.AgentSubject(sessionID)
	// Another session's messages, so the stream's last sequence is past its first.
	for range 3 {
		rig.store(t, contracts.AgentSubject("ses_resume_clamped_once_bystander"))
	}
	lastSeq := rig.streamLastSeq(t)
	streamID := rig.streamID(t)
	standIn := startSessionStandIn(t, rig.url, sessionID, answerDone)

	first := rig.subscribe(t, consumerResumeRegistration(sessionID, "epoch-1", streamID, lastSeq+pastTheEnd))
	if first.Delivery == nil || first.Delivery.ConsumerStart != lastSeq+1 {
		t.Fatalf("answer %s, want consumer_start %d, the sequence after the stream's last: this case is a session whose consumer was clamped", first.raw, lastSeq+1)
	}

	var taken []contracts.Envelope
	var floor uint64
	for range 2 {
		item, seq := rig.storeEvent(t, agent)
		taken = append(taken, item)
		floor = seq
	}
	requireFrames(t, standIn.collect(t, len(taken), 5*time.Second), taken...)
	awaitAcked(t, rig.consumers, sessionID, floor)

	rig.deregister(t, sessionID, "")
	rig.awaitNoConsumer(t, sessionID)
	var away []contracts.Envelope
	for range 3 {
		item, _ := rig.storeEvent(t, agent)
		away = append(away, item)
	}

	again := rig.subscribe(t, consumerResumeRegistration(sessionID, "epoch-2", streamID, lastSeq+pastTheEnd))

	if again.Delivery == nil || again.Delivery.ConsumerStart != floor+1 {
		t.Errorf("answer %s, want consumer_start %d, the floor %d its resume record holds plus one, not the stream's end", again.raw, floor+1, floor)
	}
	rig.requireOneLogLine(t, "INFO", loweredStartLine, sessionID, map[string]any{
		"asked_start":  lastSeq + pastTheEnd + 1,
		"record_floor": floor,
		"stream_id":    streamID,
	})
	if lines := rig.logLines(t, clampedStartLine, sessionID); len(lines) != 1 {
		t.Errorf("the listener logged %d lines %q for %s, want one, the first registration's: %v", len(lines), clampedStartLine, sessionID, lines)
	}
	requireFrames(t, standIn.collect(t, len(away), 5*time.Second), away...)
	standIn.none(t, time.Second)
}
