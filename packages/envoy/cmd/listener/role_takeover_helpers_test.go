package main

// The test helpers the takeover copy's rows share: a role claim and a role's release through the
// listener's own routes, and what the stream holds on a subject, each message with its envelope.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	natsgo "github.com/nats-io/nats.go"

	"github.com/sjawhar/envoy/internal/bus"
	"github.com/sjawhar/envoy/internal/contracts"
	"github.com/sjawhar/envoy/internal/id"
	"github.com/sjawhar/envoy/internal/store"
	"github.com/sjawhar/envoy/internal/testnats"
	"github.com/sjawhar/envoy/internal/testwait"
)

// postRoleClaim is handler's answer to POST /v1/roles/set of role by sessionID, outright or soft,
// naming instance as its process when it is not empty. It touches no testing.T, so a row may make
// it on a goroutine of its own.
func postRoleClaim(handler http.Handler, sessionID, role, instance string, soft bool) *httptest.ResponseRecorder {
	fields := map[string]any{"session_id": sessionID, "role": role}
	if soft {
		fields["soft"] = true
	}
	if instance != "" {
		fields["client_instance_id"] = instance
	}
	body, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/roles/set", string(body)))
	return recorder
}

// grantRoleClaim is postRoleClaim, and fails t unless the claim is granted.
func grantRoleClaim(t testing.TB, handler http.Handler, sessionID, role, instance string, soft bool) {
	t.Helper()
	if recorder := postRoleClaim(handler, sessionID, role, instance, soft); recorder.Code != http.StatusOK {
		t.Fatalf("%s claims %s (process %q, soft %v): status %d %s; want 200", sessionID, role, instance, soft, recorder.Code, recorder.Body.String())
	}
}

// releaseRoleThrough has sessionID's process instance hand role back through handler's POST
// /v1/interests/unsubscribe, naming the role's lane topic, which frees a claim naming that
// process, and fails t unless the route answers 200 and no claim of role stands after it, as
// registry reads it.
func releaseRoleThrough(t testing.TB, handler http.Handler, registry *store.Registry, sessionID, role, instance string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"session_id":         sessionID,
		"topics":             []string{contracts.RoleTopicPrefix + role},
		"client_instance_id": instance,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, postJSON("/v1/interests/unsubscribe", string(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("%s releases %s: status %d %s; want 200", sessionID, role, recorder.Code, recorder.Body.String())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	claim, err := registry.RoleClaim(ctx, role)
	if err != nil {
		t.Fatalf("read role %s's claim after %s released it: %v", role, sessionID, err)
	}
	if claim.HolderSessionID != "" {
		t.Fatalf("role %s's claim names %s after %s released it; want none", role, claim.HolderSessionID, sessionID)
	}
}

// storedEnvelope is one message the stream holds on a subject: its stream sequence, when the
// stream stored it, and its envelope.
type storedEnvelope struct {
	seq  uint64
	at   time.Time
	item contracts.Envelope
}

// storedEnvelopes is every message the stream holds on subject, in stream order (readStored), with
// its envelope. A read that fails, and a message no envelope decodes, fail t.
func storedEnvelopes(t testing.TB, client *bus.Client, subject string) []storedEnvelope {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	messages, err := readStored(ctx, client, subject)
	if err != nil {
		t.Fatalf("read what the stream holds on %s: %v", subject, err)
	}
	held := make([]storedEnvelope, 0, len(messages))
	for _, message := range messages {
		var item contracts.Envelope
		if err := json.Unmarshal(message.Data, &item); err != nil {
			t.Fatalf("decode the message at %d on %s: %v", message.Sequence, subject, err)
		}
		held = append(held, storedEnvelope{seq: message.Sequence, at: message.Time, item: item})
	}
	return held
}

// copiesOf is how many messages of held carry eventID.
func copiesOf(held []storedEnvelope, eventID string) int {
	copies := 0
	for _, message := range held {
		if message.item.EventID == eventID {
			copies++
		}
	}
	return copies
}

// markedFrame is a message's bytes as a row stores them on a subject, the shape it has, its
// mark: the text a row finds it by in what a session received (hearMarks) and in a role's kept
// lane (keptMarks), its payload summary where it is a JSON object with one, else its whole text -
// and, for a role message, the issued_at its sender wrote, which every copy of it must carry.
type markedFrame struct {
	shape    string
	mark     string
	data     []byte
	issuedAt int64
}

// roleFramesTheStrictReadsMiss is a role message of role for each way a stored one, as the role
// lane stores a forward to the role's holder, can be read wrongly by a read of its whole envelope:
// four that the listener's envelope decode refuses (decodeStoredEnvelope) although JSON holds them
// - a newer build's urgency, source and expects_reply words, and an issued_at written with a
// fraction of zero - and three that a read of the envelope into a struct refuses or reads as no
// role message - an event id written as a number, a UTF-8 byte order mark before the JSON, and a
// "TOPIC" key after "topic", which a read that matches keys without case takes as the topic. The
// plugins' transport reads each of them as that role message. Each one's topic, read by its exact
// key past the byte order mark, is role's topic, and t fails for a shape that does not hold so.
func roleFramesTheStrictReadsMiss(t testing.TB, role string) []markedFrame {
	t.Helper()
	topic := contracts.RoleTopicPrefix + role
	shapes := []struct {
		shape string
		edit  func(item *contracts.Envelope, fields map[string]json.RawMessage)
		bytes func(data []byte) []byte
	}{
		{shape: "a newer build's urgency", edit: func(item *contracts.Envelope, _ map[string]json.RawMessage) { item.Urgency = "critical" }},
		{shape: "a newer build's source", edit: func(item *contracts.Envelope, _ map[string]json.RawMessage) { item.Source = "linear" }},
		{shape: "a newer build's expects_reply word", edit: func(_ *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["expects_reply"] = json.RawMessage(`"maybe"`)
		}},
		{shape: "an issued_at written with a fraction of zero", edit: func(item *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["issued_at"] = json.RawMessage(strconv.FormatInt(item.IssuedAt, 10) + ".0")
		}},
		{shape: "an event id written as a number", edit: func(_ *contracts.Envelope, fields map[string]json.RawMessage) {
			fields["event_id"] = json.RawMessage(`4626`)
		}},
		{shape: "a byte order mark before the JSON", bytes: func(data []byte) []byte { return append([]byte("\xef\xbb\xbf"), data...) }},
		{shape: `a "TOPIC" key after "topic"`, bytes: func(data []byte) []byte {
			shadow := `,"TOPIC":"` + contracts.AgentTopicPrefix + `ses_topic_shadow"}`
			return append(bytes.TrimSuffix(data, []byte("}")), shadow...)
		}},
	}
	frames := make([]markedFrame, 0, len(shapes))
	for _, shape := range shapes {
		item := listenerTestEnvelope(topic, roleForwardDedupePrefix+"strict-reads-miss-"+id.New())
		item.PayloadSummary = shape.shape + " " + id.New()
		fields := map[string]json.RawMessage{}
		if shape.edit != nil {
			shape.edit(&item, fields)
		}
		data := withFields(t, item, fields)
		if shape.bytes != nil {
			data = shape.bytes(data)
		}
		if got, isString := exactField(t, data, "topic"); !isString || got != topic {
			t.Fatalf("the %s frame %s has the topic %q by its exact key; want %s", shape.shape, data, got, topic)
		}
		var strict contracts.Envelope
		if err := decodeStoredEnvelope(data, &strict); err == nil && strict.Topic == topic {
			t.Fatalf("the listener's envelope decode reads the %s frame %s as role %s's message; want it to refuse or misread it", shape.shape, data, role)
		}
		frames = append(frames, markedFrame{shape: shape.shape, mark: item.PayloadSummary, data: data, issuedAt: item.IssuedAt})
	}
	return frames
}

// framesNoReadCanTellTheRoleOf is a message for each way a stored one can be one whose role no
// read can tell (readRoleFrame): one that is not a JSON object, one whose topic is a number with
// no role mark, one whose topic is a JSON object with no role mark, and one whose role mark is a
// number under a topic that names no role. None of them is a role message to that read, so the
// relay sends each to the session whose subject holds it. t fails for a shape that does not hold
// so.
func framesNoReadCanTellTheRoleOf(t testing.TB) []markedFrame {
	t.Helper()
	notAnObject := "not a JSON object " + id.New()
	topicItem := listenerTestEnvelope("", roleForwardDedupePrefix+"topic-a-number-"+id.New())
	topicItem.PayloadSummary = "a topic that is a number " + id.New()
	numberTopic := withFields(t, topicItem, map[string]json.RawMessage{"topic": json.RawMessage(`5`)})
	objectItem := listenerTestEnvelope("", "topic-an-object-"+id.New())
	objectItem.PayloadSummary = "a topic that is a JSON object " + id.New()
	objectTopic := withFields(t, objectItem, map[string]json.RawMessage{"topic": json.RawMessage(`{"name":"notifications.role.deliverer"}`)})
	markItem := listenerTestEnvelope(contracts.AgentTopicPrefix+"ses_role_mark_a_number", "role-mark-a-number-"+id.New())
	markItem.PayloadSummary = "a role mark that is a number " + id.New()
	numberMark := withFields(t, markItem, map[string]json.RawMessage{"role": json.RawMessage(`7`)})
	var fields map[string]json.RawMessage
	if json.Unmarshal([]byte(notAnObject), &fields) == nil {
		t.Fatalf("%q reads as a JSON object; want it not to", notAnObject)
	}
	if got, isString := exactField(t, numberTopic, "topic"); isString || got != "5" {
		t.Fatalf("the frame %s has the topic %q (a string: %t); want the number 5", numberTopic, got, isString)
	}
	if _, isString := exactField(t, objectTopic, "topic"); isString {
		t.Fatalf("the frame %s has a string topic; want a JSON object", objectTopic)
	}
	if got, isString := exactField(t, numberMark, "role"); isString || got != "7" {
		t.Fatalf("the frame %s has the role mark %q (a string: %t); want the number 7", numberMark, got, isString)
	}
	frames := []markedFrame{
		{shape: "a message that is not a JSON object", mark: notAnObject, data: []byte(notAnObject)},
		{shape: "a topic that is a number, with no role mark", mark: topicItem.PayloadSummary, data: numberTopic},
		{shape: "a topic that is a JSON object, with no role mark", mark: objectItem.PayloadSummary, data: objectTopic},
		{shape: "a role mark that is a number, under a topic that names no role", mark: markItem.PayloadSummary, data: numberMark},
	}
	for _, frame := range frames {
		if _, err := readRoleFrame(frame.data); !errors.Is(err, errRoleFrameUnreadable) {
			t.Fatalf("readRoleFrame answers %v for %s; want errRoleFrameUnreadable", err, frame.shape)
		}
	}
	return frames
}

// withFields is item's JSON with each of fields set to its raw value.
func withFields(t testing.TB, item contracts.Envelope, fields map[string]json.RawMessage) []byte {
	t.Helper()
	encoded, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	if len(fields) == 0 {
		return encoded
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &all); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		all[key] = value
	}
	data, err := json.Marshal(all)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// exactField is the value data's field key holds, read by its exact key past a leading UTF-8 byte
// order mark: the string it is, or its raw JSON when it is not a string. t fails unless data is a
// JSON object.
func exactField(t testing.TB, data []byte, key string) (string, bool) {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields); err != nil {
		t.Fatalf("read %s as a JSON object: %v", data, err)
	}
	var value string
	if err := json.Unmarshal(fields[key], &value); err != nil {
		return string(fields[key]), false
	}
	return value, true
}

// markOf is the mark of a message's bytes (markedFrame): its payload summary, read by its exact
// key past a leading UTF-8 byte order mark, where it is a JSON object with one, else its whole
// text.
func markOf(data []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields) == nil {
		var summary string
		if json.Unmarshal(fields["payload_summary"], &summary) == nil && summary != "" {
			return summary
		}
	}
	return string(data)
}

// eventIDOf is the event id of a message's bytes, read by its exact key, empty where it is not a
// JSON object or its event id is not a string.
func eventIDOf(data []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields) != nil {
		return ""
	}
	var eventID string
	_ = json.Unmarshal(fields["event_id"], &eventID)
	return eventID
}

// issuedAtOf is the issued_at of a message's bytes, read by its exact key as a JSON number, and -1
// where it is not a JSON object or its issued_at is no number.
func issuedAtOf(data []byte) int64 {
	var fields map[string]json.RawMessage
	if json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &fields) != nil {
		return -1
	}
	var number json.Number
	if json.Unmarshal(fields["issued_at"], &number) != nil {
		return -1
	}
	value, err := strconv.ParseFloat(number.String(), 64)
	if err != nil {
		return -1
	}
	return int64(value)
}

// hearMarks is sessionID's process on a connection of its own: it takes each relay frame as bytes,
// records its event id and its mark (markOf) however little of it a read of the envelope would
// take, and answers it received and done, as a session that wrote it does; it answers the
// connection probe (bus.ConnectionProbeHeader) done, as the plugins do. It answers nothing more,
// and its connection drains, once t ends.
func hearMarks(t testing.TB, url, sessionID string) *heardFrames {
	t.Helper()
	conn := testnats.Connect(t, url)
	heard := &heardFrames{}
	var silent atomic.Bool
	if _, err := conn.Subscribe(bus.DeliverSubject(sessionID), func(msg *natsgo.Msg) {
		if silent.Load() {
			return
		}
		replies := []string{relayReplyReceived, relayReplyDone}
		if msg.Header.Get(bus.ConnectionProbeHeader) == "1" {
			replies = []string{relayReplyDone}
		} else {
			heard.mu.Lock()
			heard.events = append(heard.events, eventIDOf(msg.Data))
			heard.marks = append(heard.marks, markOf(msg.Data))
			heard.issuedAt = append(heard.issuedAt, issuedAtOf(msg.Data))
			heard.mu.Unlock()
		}
		// A reply that does not reach the relay leaves the frame to the relay's receipt wait,
		// which a row's own waits see; nothing here reports to t, which may have ended.
		for _, reply := range replies {
			_ = conn.Publish(msg.Reply, []byte(reply))
		}
	}); err != nil {
		t.Fatalf("subscribe %s's process to its relay subject: %v", sessionID, err)
	}
	if err := conn.Flush(); err != nil {
		t.Fatalf("flush %s's subscription: %v", sessionID, err)
	}
	t.Cleanup(func() {
		silent.Store(true)
		if err := conn.Drain(); err == nil {
			testwait.Holds(5*time.Second, testwait.Poll, conn.IsClosed)
		}
	})
	return heard
}

// keptMarks is how many messages role's kept lane holds whose mark (markOf) is mark, and how many
// it holds in all, read as bytes.
func keptMarks(t testing.TB, client *bus.Client, role, mark string) (int, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	messages, err := readStored(ctx, client, bus.KeptRoleSubject(role))
	if err != nil {
		t.Fatalf("read role %s's kept lane: %v", role, err)
	}
	marked := 0
	for _, message := range messages {
		if markOf(message.Data) == mark {
			marked++
		}
	}
	return marked, len(messages)
}
